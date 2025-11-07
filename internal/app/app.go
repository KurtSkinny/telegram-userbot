// Package app — верхний уровень сборки и инициализации пользовательского Telegram‑клиента (userbot).
// Здесь связываются конфигурация, сетевой слой (gotd/telegram), диспетчер апдейтов, очередь уведомлений
// и инфраструктурные сервисы. Отсюда стартует цикл обработки событий и обеспечивается корректный shutdown.
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	botapionotifier "telegram-userbot/internal/adapters/botapi/notifier"
	"telegram-userbot/internal/adapters/telegram/core"
	telegramnotifier "telegram-userbot/internal/adapters/telegram/notifier"
	"telegram-userbot/internal/domain/filters"
	"telegram-userbot/internal/domain/notifications"
	domainupdates "telegram-userbot/internal/domain/updates"
	"telegram-userbot/internal/infra/concurrency"
	"telegram-userbot/internal/infra/config"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/telegram/connection"
	"telegram-userbot/internal/infra/telegram/peersmgr"
	"telegram-userbot/internal/infra/telegram/session"

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
	cl        *core.ClientCore          // Авторизованный клиент gotd и его API-обёртка (Self, вызовы tg).
	filters   *filters.FilterEngine     // Движок фильтров: загрузка, хранение, матчи.
	notif     *notifications.Queue      // Асинхронная очередь уведомлений: транспорт client/bot, график, ретраи.
	dupCache  *concurrency.Deduplicator // Фильтр повторов за заданное окно (идемпотентность на уровне событий).
	debouncer *concurrency.Debouncer    // Сглаживание бурстов (частые правки одного сообщения и т.п.).
	handlers  *domainupdates.Handlers   // Доменные обработчики апдейтов и фоновые задачи.
	dispatch  *tg.UpdateDispatcher      // Маршрутизатор апдейтов gotd: OnNewMessage/OnEdit/etc.
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

// Init связывает компоненты приложения и подготавливает их к запуску:
//  1. создаёт tgupdates.Manager и диспетчер апдейтов,
//  2. настраивает telegram.Options (сессионное хранилище, хуки, DeviceConfig, DCList),
//  3. инициализирует MTProto‑клиент, кэш пиров, очередь уведомлений и таймзону,
//  4. поднимает защиту от дублей и дебаунсер,
//  5. регистрирует доменные обработчики и конструирует Runner.
//
// Возвращает ошибку, если какой-либо этап не удался.
func (a *App) Init(ctx context.Context, stop context.CancelFunc) error {
	logger.Info("Userbot initializing...")

	a.ctx = ctx
	a.stop = stop
	a.dispatch = func(d tg.UpdateDispatcher) *tg.UpdateDispatcher { return &d }(tg.NewUpdateDispatcher())
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
		SessionStorage: &session.NotifyStorage{Path: config.Env().SessionFile},
		UpdateHandler:  updateHandlerProxy,
		Middlewares: []telegram.Middleware{
			updhook.UpdateHook(func(mwCtx context.Context, updates tg.UpdatesClass) error {
				return updateHandlerProxy.Handle(mwCtx, updates)
			}),
			// connstate.Middleware(updhook.UpdateHook(a.updMgr.Handle)),
		},
		// При сообщении от gotd о «мертвом» соединении отмечаем отключение для зависимых узлов.
		OnDead: func() {
			// logger.Debug("MTProto client reported dead connection, scheduling reconnect")
			connection.MarkDisconnected()
		},
		Device: telegram.DeviceConfig{
			DeviceModel:   "MacBookPro18,1",
			SystemVersion: "macOS v15.6.1 build 24G90",
			AppVersion:    "v5.5.0",
		},
		// Logger: logger.Logger().Named("MTProto_Client"),
	}

	// Для тестовых окружений используем DC тестового стенда Telegram.
	if config.Env().TestDC {
		options.DCList = dcs.Test()
	}

	// 3) Инициализация клиента gotd на основе диспетчера апдейтов и опций.
	cl, clErr := core.New(a.dispatch, options)
	if clErr != nil {
		return fmt.Errorf("init client: %w", clErr)
	}
	a.cl = cl

	peersSvc, err := peersmgr.New(cl.API, config.Env().PeersCacheFile)
	if err != nil {
		return fmt.Errorf("init peers manager: %w", err)
	}
	if err = peersSvc.LoadFromStorage(ctx); err != nil {
		return fmt.Errorf("load peers storage: %w", err)
	}
	a.peers = peersSvc

	updConfig := tgupdates.Config{
		Handler:      a.dispatch,
		Storage:      core.NewFileStorage(config.Env().StateFile),
		AccessHasher: peersSvc.Mgr,
		// Logger:  logger.Logger().Named("Update_Manager"),
	}
	a.updMgr = tgupdates.New(updConfig)
	updateFunc = contribstorage.UpdateHook(peersSvc.Mgr.UpdateHook(a.updMgr), peersSvc.Store()).Handle

	// Инициализация filters (внутри загружает recipients)
	a.filters = filters.NewFilterEngine(config.Env().FiltersFile, config.Env().RecipientsFile)
	if filtersErr := a.filters.Init(); filtersErr != nil {
		return fmt.Errorf("load filters: %w", filtersErr)
	}
	logger.Infof("Filters loaded: %d total, %d unique chats",
		len(a.filters.GetFilters()), len(a.filters.GetUniqueChats()))

	// Подсистема уведомлений: файловые сторы для очереди и неудачных отправок.
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
		sender = telegramnotifier.NewClientSender(a.cl.API, config.Env().ThrottleRPS, a.peers)
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

	// 5) Защита от дублей и бурстов правок.
	a.dupCache = concurrency.NewDeduplicator(config.Env().DedupWindowSec)
	a.debouncer = concurrency.NewDebouncer(config.Env().DebounceEditMS)

	// 6) Регистрация доменных обработчиков, которым нужны API клиента и инфраструктура.
	h := domainupdates.NewHandlers(cl.API, a.filters, a.notif, a.dupCache, a.debouncer, a.stop, a.peers)
	a.handlers = h

	// Маршрутизация апдейтов на доменные обработчики.
	a.dispatch.OnNewMessage(h.OnNewMessage)
	a.dispatch.OnNewChannelMessage(h.OnNewChannelMessage)
	a.dispatch.OnEditMessage(h.OnEditMessage)
	a.dispatch.OnEditChannelMessage(h.OnEditChannelMessage)

	// 7) Конструируем Runner, который запустит цикл и обеспечит корректный shutdown.
	a.runner = NewRunner(a.ctx, a.stop, a.cl, a.filters, a.notif, a.dupCache, a.debouncer, a.handlers, a.peers)

	return nil
}

// Run делегирует запуск основного цикла Runner’у с уже сконфигурированным менеджером апдейтов.
func (a *App) Run() error {
	return a.runner.Run(a.updMgr)
}
