// Package app — верхний уровень сборки и инициализации пользовательского Telegram‑клиента (userbot).
// Здесь связываются конфигурация, сетевой слой (gotd/telegram), диспетчер апдейтов, очередь уведомлений
// и инфраструктурные сервисы. Отсюда стартует цикл обработки событий и обеспечивается корректный shutdown.
package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"telegram-userbot/internal/botapionotifier"
	"telegram-userbot/internal/concurrency"
	"telegram-userbot/internal/config"
	"telegram-userbot/internal/filters"
	"telegram-userbot/internal/logger"
	"telegram-userbot/internal/notifications"
	"telegram-userbot/internal/storage"
	"telegram-userbot/internal/support/version"
	"telegram-userbot/internal/telegram/connection"
	"telegram-userbot/internal/telegram/peersmgr"
	"telegram-userbot/internal/telegram/session"
	"telegram-userbot/internal/telegramnotifier"
	"telegram-userbot/internal/timeutil"
	domainupdates "telegram-userbot/internal/updates"

	"github.com/go-faster/errors"
	"go.etcd.io/bbolt"
	"golang.org/x/time/rate"

	boltstor "github.com/gotd/contrib/bbolt"
	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/contrib/middleware/ratelimit"
	contribstorage "github.com/gotd/contrib/storage"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	tgupdates "github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
)

// lazyUpdateHandler — это обёртка, которая позволяет отложить установку
// реального обработчика апдейтов, разрывая цикл инициализации.
type lazyUpdateHandler struct {
	mu      sync.RWMutex
	handler telegram.UpdateHandler
}

func (h *lazyUpdateHandler) Handle(ctx context.Context, u tg.UpdatesClass) error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.handler != nil {
		return h.handler.Handle(ctx, u)
	}
	return nil
}

func (h *lazyUpdateHandler) set(realHandler telegram.UpdateHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handler = realHandler
}

// App агрегирует зависимости userbot и управляет их связью.
// Отвечает за:
//   - конфигурацию и телеграм‑клиента (авторизация, API),
//   - подсистему уведомлений и её хранилища, расписание и таймзону,
//   - защиту от дублей и сглаживание частых правок,
//   - маршрутизацию апдейтов и регистрацию доменных обработчиков,
//   - запуск Runner, который оркестрирует жизненный цикл и graceful shutdown.
type App struct {
	cfg        *config.Config            // Конфигурация приложения
	mainCtx    context.Context           // Контекст жизненного цикла приложения.
	mainCancel context.CancelFunc        // Инициирует отмену mainCtx.
	filters    *filters.FilterEngine     // Движок фильтров: загрузка, хранение, матчи.
	notif      *notifications.Queue      // Асинхронная очередь уведомлений: транспорт client/bot, график, ретраи.
	dupCache   *concurrency.Deduplicator // Фильтр повторов за заданное окно (идемпотентность на уровне событий).
	debouncer  *concurrency.Debouncer    // Сглаживание бурстов (частые правки одного сообщения и т.п.).
	handlers   *domainupdates.Handlers   // Доменные обработчики апдейтов и фоновые задачи.
	runner     *Runner                   // Оркестратор жизненного цикла и CLI.
	updMgr     *tgupdates.Manager        // Менеджер апдейтов gotd: поток событий и локальное состояние.
	peers      *peersmgr.Service         // Менеджер пиров + persist storage.
	waiter     *floodwait.Waiter         // Middleware для обработки FLOOD_WAIT.
}

// CleanPeriodHours — периодичность очистки внутренних фильтров/кэшей уведомлений (часы),
// чтобы не накапливать устаревшие записи во время длительной работы.
const (
	CleanPeriodHours = 24
	notifierClient   = "client"
	notifierBot      = "bot"
)

// NewApp создаёт пустой каркас приложения. Фактическая инициализация выполняется в Init().
func NewApp(mainCtx context.Context, mainCancel context.CancelFunc, cfg *config.Config) *App {
	return &App{
		cfg:        cfg,
		mainCtx:    mainCtx,
		mainCancel: mainCancel,
	}
}

// Run запускает основной цикл приложения: инициализацию клиента, менеджера апдейтов,
// доменных обработчиков и прочих сервисов, а затем стартует Runner,
// который оркестрирует жизненный цикл и корректное завершение работы.
// Блокируется до остановки приложения и возвращает ошибку, если что-то пошло не так.
func (a *App) Run() error {
	logger.Info("Userbot initializing...")

	dispatcher := tg.NewUpdateDispatcher()
	lazyHandler := &lazyUpdateHandler{}
	a.waiter = floodwait.NewWaiter()

	// 1) Опции MTProto‑клиента: сессии, хуки апдейтов, поведение при dead‑соединении и паспорт устройства.
	options := telegram.Options{
		SessionStorage: &session.FileStorage{Path: a.cfg.GetEnv().SessionFile},
		UpdateHandler:  lazyHandler,
		Middlewares: []telegram.Middleware{
			a.waiter,
			ratelimit.New(
				rate.Limit(a.cfg.GetEnv().ThrottleRPS),
				a.cfg.GetEnv().ThrottleRPS*2, //nolint:mnd // burst = 2*rate
			),
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
	if a.cfg.GetEnv().TestDC {
		options.DCList = dcs.Test()
	}

	// Инициализация клиента gotd
	client := telegram.NewClient(a.cfg.GetEnv().APIID, a.cfg.GetEnv().APIHash, options)

	peersSvc, peersMgrErr := peersmgr.New(client.API(), a.cfg.GetEnv().PeersCacheFile)
	if peersMgrErr != nil {
		return fmt.Errorf("init peers manager: %w", peersMgrErr)
	}
	if err := peersSvc.LoadFromStorage(a.mainCtx); err != nil {
		return fmt.Errorf("load peers storage: %w", err)
	}
	a.peers = peersSvc

	// Инициализация хранилища состояния апдейтов
	if err := storage.EnsureDir(a.cfg.GetEnv().StateFile); err != nil {
		return fmt.Errorf("ensure state file dir: %w", err)
	}
	stateStorageBoltdb, err := bbolt.Open(a.cfg.GetEnv().StateFile, storage.DefaultFilePerm, nil)
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

	// Устанавливаем реальный обработчик в lazyHandler
	realHandler := contribstorage.UpdateHook(peersSvc.Mgr.UpdateHook(a.updMgr), peersSvc.Store())
	lazyHandler.set(realHandler)

	// Инициализация filters
	a.filters = filters.NewFilterEngine(a.cfg.GetEnv().FiltersFile, a.cfg.GetEnv().RecipientsFile)
	if filtersErr := a.filters.Load(); filtersErr != nil {
		return fmt.Errorf("load filters: %w", filtersErr)
	}

	// Подсистема уведомлений
	queueStore, err := notifications.NewQueueStore(a.cfg.GetEnv().NotifyQueueFile, time.Second)
	if err != nil {
		return fmt.Errorf("init queue store: %w", err)
	}
	failedStore, err := notifications.NewFailedStore(a.cfg.GetEnv().NotifyFailedFile)
	if err != nil {
		return fmt.Errorf("init failed store: %w", err)
	}

	// Таймзона для расписания уведомлений берётся из конфигурации.
	loc, err := timeutil.ParseLocation(a.cfg.GetEnv().NotifyTimezone)
	if err != nil {
		return fmt.Errorf("load notify timezone: %w", err)
	}

	// Выбор транспорта уведомлений: client (userbot) или bot (Bot API).
	var sender notifications.PreparedSender
	switch a.cfg.GetEnv().Notifier {
	case notifierClient:
		sender = telegramnotifier.NewClientSender(client.API(), a.cfg.GetEnv().ThrottleRPS, a.peers)
	case notifierBot:
		sender = botapionotifier.NewBotSender(a.cfg.GetEnv().BotToken, a.cfg.GetEnv().TestDC, a.cfg.GetEnv().ThrottleRPS)
	default:
		return errors.New(`invalid NOTIFIER option in .env (must be "client" or "bot")`)
	}

	// Сборка очереди уведомлений: транспорт, сторы, расписание, таймзона, часы.
	queue, err := notifications.NewQueue(notifications.QueueOptions{
		Sender:   sender,
		Store:    queueStore,
		Failed:   failedStore,
		Schedule: a.cfg.GetEnv().NotifySchedule,
		Location: loc,
		Clock:    time.Now,
		Peers:    a.peers,
	})
	if err != nil {
		return fmt.Errorf("init notifications queue: %w", err)
	}
	a.notif = queue

	// Защита от дублей и бурстов правок.
	a.dupCache = concurrency.NewDeduplicator(a.cfg.GetEnv().DedupWindowSec)
	a.debouncer = concurrency.NewDebouncer(a.cfg.GetEnv().DebounceEditMS)

	// Регистрация доменных обработчиков, которым нужны API клиента и инфраструктура.
	h := domainupdates.NewHandlers(a.cfg, client.API(), a.filters, a.notif, a.dupCache, a.debouncer, a.mainCancel, a.peers)
	a.handlers = h

	// Маршрутизация апдейтов на доменные обработчики.
	dispatcher.OnNewMessage(h.OnNewMessage)
	dispatcher.OnNewChannelMessage(h.OnNewChannelMessage)
	dispatcher.OnEditMessage(h.OnEditMessage)
	dispatcher.OnEditChannelMessage(h.OnEditChannelMessage)

	// Конструируем Runner, который запустит цикл и обеспечит корректный shutdown.
	a.runner = NewRunner(
		a.mainCtx,
		a.mainCancel,
		a.cfg,
		client,
		a.filters,
		a.notif,
		a.dupCache,
		a.debouncer,
		a.handlers,
		a.peers,
	)

	return a.runner.Run(a.waiter, a.updMgr)
}
