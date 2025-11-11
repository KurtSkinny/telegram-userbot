// Package app — верхний уровень сборки и инициализации пользовательского Telegram‑клиента (userbot).
// Здесь связываются конфигурация, сетевой слой (gotd/telegram), диспетчер апдейтов, очередь уведомлений
// и инфраструктурные сервисы. Отсюда стартует цикл обработки событий и обеспечивается корректный shutdown.
package app

import (
	"context"
	"fmt"
	"time"

	botapionotifier "telegram-userbot/internal/adapters/botapi/notifier"
	telegramnotifier "telegram-userbot/internal/adapters/telegram/notifier"
	"telegram-userbot/internal/domain/filters"
	"telegram-userbot/internal/domain/notifications"
	domainupdates "telegram-userbot/internal/domain/updates"
	"telegram-userbot/internal/infra/concurrency"
	"telegram-userbot/internal/infra/config"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/storage"
	"telegram-userbot/internal/infra/telegram/connection"
	"telegram-userbot/internal/infra/telegram/peersmgr"
	"telegram-userbot/internal/infra/telegram/session"
	"telegram-userbot/internal/support/version"

	"github.com/go-faster/errors"
	"go.etcd.io/bbolt"

	boltstor "github.com/gotd/contrib/bbolt"
	contribstorage "github.com/gotd/contrib/storage"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	tgupdates "github.com/gotd/td/telegram/updates"
	updhook "github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/tg"
)

// App агрегирует зависимости userbot и управляет их связью.
// Отвечает за:
//   - конфигурацию и телеграм‑клиента (авторизация, API),
//   - подсистему уведомлений и её хранилища, расписание и таймзону,
//   - защиту от дублей и сглаживание частых правок,
//   - маршрутизацию апдейтов и регистрацию доменных обработчиков,
//   - запуск Runner, который оркестрирует жизненный цикл и graceful shutdown.
type App struct {
	filters   *filters.FilterEngine     // Движок фильтров: загрузка, хранение, матчи.
	notif     *notifications.Queue      // Асинхронная очередь уведомлений: транспорт client/bot, график, ретраи.
	dupCache  *concurrency.Deduplicator // Фильтр повторов за заданное окно (идемпотентность на уровне событий).
	debouncer *concurrency.Debouncer    // Сглаживание бурстов (частые правки одного сообщения и т.п.).
	handlers  *domainupdates.Handlers   // Доменные обработчики апдейтов и фоновые задачи.
	runner    *Runner                   // Оркестратор жизненного цикла и CLI.
	updMgr    *tgupdates.Manager        // Менеджер апдейтов gotd: поток событий и локальное состояние.
	peers     *peersmgr.Service         // Менеджер пиров + persist storage.
	ctx       context.Context           // Внешний контекст приложения (отменяется по сигналам/CLI).
	stop      context.CancelFunc        // Инициирует общий shutdown.
}

// CleanPeriodHours — периодичность очистки внутренних фильтров/кэшей уведомлений (часы),
// чтобы не накапливать устаревшие записи во время длительной работы.
const (
	CleanPeriodHours = 24
	notifierClient   = "client"
	notifierBot      = "bot"
)

// NewApp создаёт пустой каркас приложения. Фактическая инициализация выполняется в Init().
func NewApp() *App {
	return &App{}
}

// Init связывает компоненты приложения и подготавливает их к запуску
func (a *App) Init(ctx context.Context, stop context.CancelFunc) error {
	logger.Info("Userbot initializing...")

	a.ctx = ctx
	a.stop = stop
	dispatcher := tg.NewUpdateDispatcher()
	var updateFunc func(context.Context, tg.UpdatesClass) error
	updateHandlerProxy := telegram.UpdateHandlerFunc(func(handlerCtx context.Context, updates tg.UpdatesClass) error {
		if updateFunc != nil {
			return updateFunc(handlerCtx, updates)
		}
		if a.updMgr != nil {
			return a.updMgr.Handle(handlerCtx, updates)
		}
		return nil
	})

	// 1) Опции MTProto‑клиента: сессии, хуки апдейтов, поведение при dead‑соединении и паспорт устройства.
	options := telegram.Options{
		SessionStorage: &session.FileStorage{Path: config.Env().SessionFile},
		UpdateHandler:  updateHandlerProxy,
		Middlewares: []telegram.Middleware{
			updhook.UpdateHook(func(mwCtx context.Context, updates tg.UpdatesClass) error {
				return updateHandlerProxy.Handle(mwCtx, updates)
			}),
			// connstate.Middleware(updhook.UpdateHook(a.updMgr.Handle)),
		},
		// При сообщении от gotd о «мертвом» соединении отмечаем отключение для зависимых узлов.
		OnDead: func() {
			connection.MarkDisconnected()
		},
		Device: telegram.DeviceConfig{
			DeviceModel:   "MacBookPro18,1",
			SystemVersion: "macOS v15.6.1 build 24G90",
			AppVersion:    version.Version,
		},
		// Logger: logger.Logger().Named("MTProto_Client"),
	}

	// Для тестовых окружений используем DC тестового стенда Telegram.
	if config.Env().TestDC {
		options.DCList = dcs.Test()
	}

	// Инициализация клиента gotd
	client := telegram.NewClient(config.Env().APIID, config.Env().APIHash, options)

	peersSvc, peersMgrErr := peersmgr.New(client.API(), config.Env().PeersCacheFile)
	if peersMgrErr != nil {
		return fmt.Errorf("init peers manager: %w", peersMgrErr)
	}
	if err := peersSvc.LoadFromStorage(ctx); err != nil {
		return fmt.Errorf("load peers storage: %w", err)
	}
	a.peers = peersSvc

	// Инициализация хранилища состояния апдейтов
	if err := storage.EnsureDir(config.Env().StateFile); err != nil {
		return fmt.Errorf("ensure state file dir: %w", err)
	}
	stateStorageBoltdb, err := bbolt.Open(config.Env().StateFile, storage.DefaultFilePerm, nil)
	if err != nil {
		return errors.Wrap(err, "create bolt storage")
	}
	stateStorage := boltstor.NewStateStorage(stateStorageBoltdb)

	// Инициализация менеджера апдейтов
	updConfig := tgupdates.Config{
		Handler:      dispatcher,
		Storage:      stateStorage,
		AccessHasher: peersSvc.Mgr,
		// Logger:  logger.Logger().Named("Update_Manager"),
	}
	a.updMgr = tgupdates.New(updConfig)
	updateFunc = contribstorage.UpdateHook(peersSvc.Mgr.UpdateHook(a.updMgr), peersSvc.Store()).Handle

	// Инициализация filters
	a.filters = filters.NewFilterEngine(config.Env().FiltersFile, config.Env().RecipientsFile)
	if filtersErr := a.filters.Init(); filtersErr != nil {
		return fmt.Errorf("load filters: %w", filtersErr)
	}
	logger.Infof("Filters loaded: %d total, %d unique chats",
		len(a.filters.GetFilters()), len(a.filters.GetUniqueChats()))

	// Подсистема уведомлений
	queueStore, err := notifications.NewQueueStore(config.Env().NotifyQueueFile, time.Second)
	if err != nil {
		return fmt.Errorf("init queue store: %w", err)
	}
	failedStore, err := notifications.NewFailedStore(config.Env().NotifyFailedFile)
	if err != nil {
		return fmt.Errorf("init failed store: %w", err)
	}

	// Таймзона для расписания уведомлений берётся из конфигурации.
	loc, err := config.ParseLocation(config.Env().NotifyTimezone)
	if err != nil {
		return fmt.Errorf("load notify timezone: %w", err)
	}

	// Выбор транспорта уведомлений: client (userbot) или bot (Bot API).
	var sender notifications.PreparedSender
	switch config.Env().Notifier {
	case notifierClient:
		sender = telegramnotifier.NewClientSender(client.API(), config.Env().ThrottleRPS, a.peers)
	case notifierBot:
		sender = botapionotifier.NewBotSender(config.Env().BotToken, config.Env().TestDC, config.Env().ThrottleRPS)
	default:
		return errors.New(`invalid NOTIFIER option in .env (must be "client" or "bot")`)
	}

	// Сборка очереди уведомлений: транспорт, сторы, расписание, таймзона, часы.
	queue, err := notifications.NewQueue(notifications.QueueOptions{
		Sender:   sender,
		Store:    queueStore,
		Failed:   failedStore,
		Schedule: config.Env().NotifySchedule,
		Location: loc,
		Clock:    time.Now,
		Peers:    a.peers,
	})
	if err != nil {
		return fmt.Errorf("init notifications queue: %w", err)
	}
	a.notif = queue

	// Защита от дублей и бурстов правок.
	a.dupCache = concurrency.NewDeduplicator(config.Env().DedupWindowSec)
	a.debouncer = concurrency.NewDebouncer(config.Env().DebounceEditMS)

	// Регистрация доменных обработчиков, которым нужны API клиента и инфраструктура.
	h := domainupdates.NewHandlers(client.API(), a.filters, a.notif, a.dupCache, a.debouncer, a.stop, a.peers)
	a.handlers = h

	// Маршрутизация апдейтов на доменные обработчики.
	dispatcher.OnNewMessage(h.OnNewMessage)
	dispatcher.OnNewChannelMessage(h.OnNewChannelMessage)
	dispatcher.OnEditMessage(h.OnEditMessage)
	dispatcher.OnEditChannelMessage(h.OnEditChannelMessage)

	// Конструируем Runner, который запустит цикл и обеспечит корректный shutdown.
	a.runner = NewRunner(a.ctx, a.stop, client, a.filters, a.notif, a.dupCache, a.debouncer, a.handlers, a.peers)

	return nil
}

// Run делегирует запуск основного цикла Runner’у с уже сконфигурированным менеджером апдейтов.
func (a *App) Run() error {
	return a.runner.Run(a.updMgr)
}
