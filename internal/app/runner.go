// Package app реализует верхний уровень управления жизненным циклом Telegram‑клиента (userbot).
// Файл runner.go — точка оркестрации: здесь собирается граф узлов (lifecycle.Manager),
// выполняется авторизация, стартует менеджер обновлений, и организуется корректный graceful shutdown.
// Бизнес‑назначение: гарантировать стабильный запуск и предсказуемое завершение работы бота так,
// чтобы доменные сервисы успели завершить операции (статусы online/offline, доставка сообщений из очереди),
// а MTProto‑движок оставался жив до отправки критичных сигналов (например, AccountUpdateStatus(offline)).
package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"telegram-userbot/internal/adapters/cli"
	"telegram-userbot/internal/adapters/telegram/core"
	"telegram-userbot/internal/domain/filters"
	"telegram-userbot/internal/domain/notifications"
	domainupdates "telegram-userbot/internal/domain/updates"
	"telegram-userbot/internal/infra/concurrency"
	"telegram-userbot/internal/infra/config"
	"telegram-userbot/internal/infra/lifecycle"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/telegram/cache"
	"telegram-userbot/internal/infra/telegram/connection"

	"telegram-userbot/internal/infra/telegram/status"

	tgupdates "github.com/gotd/td/telegram/updates"
	"go.uber.org/zap"
)

// Runner инкапсулирует сценарий запуска и остановки Telegram‑клиента и связанных подсистем.
// Отвечает за:
//   - авторизацию и идентификацию текущего пользователя (self),
//   - сборку и запуск узлов через lifecycle.Manager с учётом зависимостей,
//   - корректное завершение: сначала останавливаются узлы (статусы/очереди), затем гасится MTProto‑движок,
//   - интеграцию с CLI и доменными обработчиками обновлений.
type Runner struct {
	cl      *core.ClientCore          // Обёртка над MTProto‑клиентом и API: логин, Self(), API-интерфейс.
	filters *filters.FilterEngine     // Движок фильтров: загрузка, хранение, матчи.
	notif   *notifications.Queue      // Асинхронная очередь нотификаций (доставка сообщений администратору/сервисам).
	dedup   *concurrency.Deduplicator // Защита от повторной обработки событий (идемпотентность на уровне сигналов).
	deb     *concurrency.Debouncer    // Сглаживание/слияние частых событий (например, всплесков апдейтов).
	h       *domainupdates.Handlers   // Композиция доменных обработчиков апдейтов Telegram.
	ctx     context.Context           // Внешний контекст процесса: отменяется по Ctrl+C/сигналам.
	stop    context.CancelFunc        // Функция, инициирующая общий shutdown (используется из узлов).
}

// NewRunner подготавливает Runner с переданными зависимостями: ядро клиента, очередь уведомлений,
// утилиты конкуррентности и доменные обработчики. Возвращает объект, готовый к запуску Run().
func NewRunner(
	ctx context.Context,
	stop context.CancelFunc,
	cl *core.ClientCore,
	filters *filters.FilterEngine,
	notif *notifications.Queue,
	dedup *concurrency.Deduplicator,
	debouncer *concurrency.Debouncer,
	handlers *domainupdates.Handlers,
) *Runner {
	return &Runner{
		ctx:     ctx,
		stop:    stop,
		cl:      cl,
		filters: filters,
		notif:   notif,
		dedup:   dedup,
		deb:     debouncer,
		h:       handlers,
	}
}

// Run — главный цикл userbot. Выполняет логин, сборку и запуск узлов, стартует updates.Manager
// и управляет корректным завершением. Блокируется до завершения клиентского контекста.
// Важно: используется отдельный контекст для MTProto‑движка, чтобы дать шанс статусам/очередям
// корректно завершиться до гашения сетевого уровня.
func (r *Runner) Run(updmgr *tgupdates.Manager) error {
	clientCtx, clientCancel := context.WithCancel(context.Background())

	// Отдельный контекст именно для MTProto‑движка. Мы не привязываем его к r.ctx,
	// чтобы уметь сначала останавливать прикладные узлы (status/queue/handlers),
	// а уже затем отменять сетевой слой. Это устраняет гонку: "engine закрыт → статус offline не ушёл".

	defer clientCancel()

	return r.cl.Client.Run(clientCtx, func(ctx context.Context) error {
		// Точка входа в клиентскую сессию. Всё внутри выполняется, пока жив clientCtx.

		logger.Info("Userbot running...")

		// Загружаем профиль текущего аккаунта (Self). Нужен для логов и параметров updates.Manager.
		if loginErr := r.cl.Login(ctx); loginErr != nil {
			return loginErr
		}

		self, selfErr := r.cl.Client.Self(ctx)
		if selfErr != nil {
			return selfErr
		}

		logger.Logger().Info("Logged in as:",
			zap.String("FirstName", self.FirstName),
			zap.String("Username", self.Username),
			zap.Int64("ID", self.ID),
		)

		// todo: заменить на готовый peerscache manager из contrib
		// Прогреваем кэш пиров заранее: обработчики обновлений смогут быстрее резолвить участников/диалоги.
		if cacheErr := cache.BuildPeerCache(); cacheErr != nil {
			logger.Errorf("failed to build peer cache: %v", cacheErr)
			if config.Env().Notifier != "bot" {
				logger.Error("peer cache error, cant use client notifier")
				return cacheErr
			}
		}

		logger.Debug("BuildPeerCache: ok")

		// lifecycle.Manager управляет узлами с зависимостями: Register → StartAll → Shutdown.
		// Здесь контекстом для узлов служит контекст MTProto‑движка, чтобы узлы завершались раньше engine.

		lc := lifecycle.New(ctx)

		if err := r.registerClientNodes(ctx, lc, updmgr, self.ID); err != nil {
			return err
		}

		if err := lc.StartAll(); err != nil {
			_ = lc.Shutdown()
			return err
		}

		// ОРКЕСТРАЦИЯ ЗАВЕРШЕНИЯ:
		// Используем sync.WaitGroup.Go для симметрии Add/Done и читаемости.
		// Порядок: 1) ждём r.ctx.Done(); 2) мягко глушим узлы lc.Shutdown(); 3) отменяем clientCtx.
		// Ждём отмены внешнего контекста r.ctx (Ctrl+C или r.stop()),
		// инициализируем управляемый shutdown узлов, и только после его завершения
		// отменяем контекст MTProto-движка (clientCancel). Это даёт шанс StatusManager
		// отправить AccountUpdateStatus(offline), пока движок ещё жив.
		var wg sync.WaitGroup
		shutdownTriggered := make(chan struct{})
		wg.Go(func() {
			<-r.ctx.Done()
			_ = lc.Shutdown() // запускаем остановку узлов при ещё живом engine
			clientCancel()    // после остановки узлов — гасим движок
			close(shutdownTriggered)
		})

		// Дожидаемся завершения clientCtx (engine). Это произойдёт либо после
		// нашего clientCancel(), либо из-за внутренней ошибки движка.
		<-ctx.Done()

		// Если shutdown не был инициирован внешним контекстом (например, engine умер сам),
		// корректно остановим узлы здесь.
		select {
		case <-shutdownTriggered:
			// уже остановили выше
		default:
			_ = lc.Shutdown()
		}
		wg.Wait()

		// Возвращаем ошибку контекста движка (для логов верхнего уровня).
		return ctx.Err()
	})
}

// handleUpdatesManagerStart вызывается updates.Manager при старте обработки апдейтов.
// Здесь выполняем действия, зависящие от готовности подписки на обновления:
//   - переключение в online-статус при конфигурации notifier=="client",
//   - отправка сервисного уведомления (оставлено закомментированным, но готово к использованию).
func (r *Runner) handleUpdatesManagerStart(ctx context.Context) {
	// переходим в онлайн, если notifier == "client"
	if config.Env().Notifier == "client" {
		status.GoOnline()
	}

	logger.Debug("Updates manager started")

	// Отправляем администратору уведомление о старте сервиса, чтобы зафиксировать успешный запуск.
	// if config.Env().AdminUID > 0 {
	// 	if err := r.notif.Send(
	// 		ctx,
	// 		int64(config.Env().AdminUID),
	// 		fmt.Sprintf("%s v%s started", versioninfo.Name, versioninfo.Version),
	// 	); err != nil {
	// 		logger.Errorf("failed to send message on start: %v", err)
	// 	}
	// }
}

// registerClientNodes описывает граф узлов lifecycle.Manager, их зависимости и процедуры
// запуска/остановки. Важные моменты порядка:
//   - connection_manager должен стартовать до status_manager и очередей, т.к. им нужен живой клиент;
//   - notifications_queue и domain_handlers зависят от соединения, чтобы гарантировать доставку;
//   - updates_manager стартует после status_manager, чтобы иметь возможность перейти online в OnStart;
//   - CLI запускается отдельно и не блокирует основной цикл.
func (r *Runner) registerClientNodes(
	_ context.Context,
	lc *lifecycle.Manager,
	updmgr *tgupdates.Manager,
	selfID int64,
) error {
	// Узел: connection_manager
	// Инициализирует и публикует текущее соединение/контекст клиента для других подсистем.
	// Без зависимостей, так как сам предоставляет базовую инфраструктуру.
	if err := lc.Register(
		"connection_manager",
		"",
		nil,
		func(nodeCtx context.Context) (context.Context, error) {
			connection.Init(nodeCtx, r.cl.Client)
			return nodeCtx, nil
		},
		func(context.Context) error {
			connection.Shutdown()
			return nil
		},
	); err != nil {
		return err
	}

	// Узел: status_manager
	// Управляет статусами аккаунта (online/offline/typing). Зависит от connection_manager,
	// поскольку отправляет методы, требующие живого API и корректного контекста соединения.
	if err := lc.Register(
		"status_manager",
		"connection_manager",
		nil,
		func(nodeCtx context.Context) (context.Context, error) {
			status.Init(nodeCtx, r.cl.API)
			return nodeCtx, nil
		},
		func(context.Context) error {
			status.Shutdown()
			return nil
		},
	); err != nil {
		return err
	}

	// Узел: deduplicator
	// Глобальный фильтр повторов. Не зависит от соединения, но должен жить пока обрабатываем апдейты.
	if err := lc.Register(
		"deduplicator",
		"",
		nil,
		func(nodeCtx context.Context) (context.Context, error) {
			r.dedup.Start(nodeCtx)
			return nodeCtx, nil
		},
		func(context.Context) error {
			r.dedup.Stop()
			return nil
		},
	); err != nil {
		return err
	}

	// Узел: debouncer
	// Сглаживание всплесков событий (например, батчи typing/online). Похож по жизненному циклу на deduplicator.
	if err := lc.Register(
		"debouncer",
		"",
		nil,
		func(nodeCtx context.Context) (context.Context, error) {
			r.deb.Start(nodeCtx)
			return nodeCtx, nil
		},
		func(context.Context) error {
			r.deb.Stop()
			return nil
		},
	); err != nil {
		return err
	}

	// Узел: notifications_queue
	// Асинхронная доставка сообщений (админу/сервисам). Зависит от соединения, чтобы иметь возможность
	// отправить накопленное перед выключением. На остановке ждёт до queueShutdownTimeout.
	const queueShutdownTimeout = 15 * time.Second
	if err := lc.Register(
		"notifications_queue",
		"connection_manager",
		nil,
		func(nodeCtx context.Context) (context.Context, error) {
			r.notif.Start(nodeCtx)
			return nodeCtx, nil
		},
		func(context.Context) error {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), queueShutdownTimeout)
			defer cancel()
			return r.notif.Close(shutdownCtx)
		},
	); err != nil {
		return err
	}

	// Узел: domain_handlers
	// Композиция бизнес‑обработчиков апдейтов телеграма. Запускается после очереди нотификаций,
	// а также после инфраструктурных фильтров (dedup/debounce). При остановке аккуратно гасится.
	if err := lc.Register(
		"domain_handlers",
		"notifications_queue",
		[]string{"deduplicator", "debouncer"},
		func(nodeCtx context.Context) (context.Context, error) {
			if r.h != nil {
				r.h.Start(nodeCtx, CleanPeriodHours*time.Hour)
			}
			return nodeCtx, nil
		},
		func(context.Context) error {
			if r.h != nil {
				r.h.Stop()
			}
			return nil
		},
	); err != nil {
		return err
	}

	var updatesWG sync.WaitGroup
	updatesStart := func(nodeCtx context.Context) (context.Context, error) {
		// Узел: updates_manager (старт)
		// Запускаем updates.Manager из gotd. Передаём OnStart хук, который переводит систему в online
		// и выполняет прогрев/уведомления. Ошибки, отличные от context.Canceled, логируем.
		// После завершения менеджера (по ошибке или отмене) инициируем общий shutdown через r.stop().
		updatesWG.Go(func() {
			logger.Debug("updates_manager node: Run started")
			mgrErr := updmgr.Run(nodeCtx, r.cl.API, selfID, tgupdates.AuthOptions{
				Forget:  false,
				OnStart: r.handleUpdatesManagerStart,
			})
			if mgrErr != nil && !errors.Is(mgrErr, context.Canceled) {
				logger.Errorf("updmgr.Run return: %v", mgrErr)
			}
			logger.Debugf("updates_manager node: Run finished (err=%v)", mgrErr)
			// По завершении обновлений инициируем общий shutdown.
			r.stop()
		})
		return nodeCtx, nil
	}
	updatesStop := func(context.Context) error {
		// logger.Debug("updates_manager node: waiting for goroutine to finish")
		updatesWG.Wait()
		// logger.Debug("updates_manager node: stopped")
		return nil
	}

	// Узел: updates_manager
	// Источник событий Telegram. Должен стартовать, когда уже готовы статус и очередь, чтобы
	// корректно обрабатывать online и исходящие.
	if err := lc.Register(
		"updates_manager",
		"notifications_queue",
		[]string{"status_manager"},
		updatesStart,
		updatesStop,
	); err != nil {
		return err
	}

	// Узел: cli
	// Сервис интерактивных команд. Не блокирует основную петлю, но может инициировать shutdown через r.stop().
	cliService := cli.NewService(r.cl, r.stop, r.filters, r.notif)
	if err := lc.Register(
		"cli",
		"",
		nil,
		func(nodeCtx context.Context) (context.Context, error) {
			cliService.Start(nodeCtx)
			return nodeCtx, nil
		},
		func(context.Context) error {
			cliService.Stop()
			return nil
		},
	); err != nil {
		return err
	}

	return nil
}
