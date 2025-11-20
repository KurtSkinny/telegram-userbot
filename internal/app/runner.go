// Package app реализует верхний уровень управления жизненным циклом Telegram‑клиента (userbot).
// Файл runner.go — точка оркестрации: здесь запускаются сервисы в правильном порядке,
// выполняется авторизация, стартует менеджер обновлений, и организуется корректный graceful shutdown.
// Бизнес‑назначение: гарантировать стабильный запуск и предсказуемое завершение работы бота так,
// чтобы доменные сервисы успели завершить операции (статусы online/offline, доставка сообщений из очереди),
// а MTProto‑движок оставался жив до отправки критичных сигналов (например, AccountUpdateStatus(offline)).
package app

import (
	"context"
	"sync"
	"time"

	"telegram-userbot/internal/adapters/cli"
	"telegram-userbot/internal/adapters/telegram/core"
	"telegram-userbot/internal/adapters/web"
	"telegram-userbot/internal/domain/commands"
	"telegram-userbot/internal/domain/filters"
	"telegram-userbot/internal/domain/notifications"
	domainupdates "telegram-userbot/internal/domain/updates"
	"telegram-userbot/internal/infra/concurrency"
	"telegram-userbot/internal/infra/config"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/telegram/connection"
	"telegram-userbot/internal/infra/telegram/peersmgr"

	"telegram-userbot/internal/infra/telegram/status"

	"github.com/go-faster/errors"
	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	tgupdates "github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
)

// Runner инкапсулирует сценарий запуска и остановки Telegram‑клиента и связанных подсистем.
// Отвечает за:
//   - авторизацию и идентификацию текущего пользователя (self),
//   - линейный запуск сервисов в правильном порядке,
//   - корректное завершение: сначала останавливаются сервисы (статусы/очереди), затем гасится MTProto‑движок,
//   - интеграцию с CLI и доменными обработчиками обновлений.
type Runner struct {
	client        *telegram.Client          // Обёртка над MTProto‑клиентом и API: логин, Self(), API-интерфейс.
	filters       *filters.FilterEngine     // Движок фильтров: загрузка, хранение, матчи.
	notif         *notifications.Queue      // Асинхронная очередь нотификаций (доставка сообщений администратору/сервисам).
	dedup         *concurrency.Deduplicator // Защита от повторной обработки событий (идемпотентность на уровне сигналов).
	deb           *concurrency.Debouncer    // Сглаживание/слияние частых событий (например, всплесков апдейтов).
	handlers      *domainupdates.Handlers   // Композиция доменных обработчиков апдейтов Telegram.
	mainCtx       context.Context           // Внешний контекст процесса: отменяется по Ctrl+C/сигналам.
	mainCancel    context.CancelFunc        // Функция, инициирующая общий shutdown (используется из узлов).
	peers         *peersmgr.Service         // Сервис пиров (peers.Manager + persist storage).
	cmdExecutor   commands.Executor         // Исполнитель команд (используется CLI и Web).
	cliService    *cli.Service              // CLI сервис для интерактивных команд.
	webServer     *web.Server               // Web-сервер для управления через браузер.
	updatesWG     sync.WaitGroup            // WaitGroup для updates_manager.
	updatesCancel context.CancelFunc        // Функция отмены контекста для updates_manager.
}

const (
	webServerShutdownTimeout = 10 * time.Second
)

// NewRunner подготавливает Runner с переданными зависимостями: ядро клиента, очередь уведомлений,
// утилиты конкуррентности и доменные обработчики. Возвращает объект, готовый к запуску Run().
func NewRunner(
	mainCtx context.Context,
	mainCancel context.CancelFunc,
	client *telegram.Client,
	filters *filters.FilterEngine,
	notif *notifications.Queue,
	dedup *concurrency.Deduplicator,
	debouncer *concurrency.Debouncer,
	handlers *domainupdates.Handlers,
	peers *peersmgr.Service,
) *Runner {
	return &Runner{
		mainCtx:    mainCtx,
		mainCancel: mainCancel,
		client:     client,
		filters:    filters,
		notif:      notif,
		dedup:      dedup,
		deb:        debouncer,
		handlers:   handlers,
		peers:      peers,
	}
}

// Run — главный цикл userbot. Выполняет логин, сборку и запуск узлов, стартует updates.Manager
// и управляет корректным завершением. Блокируется до завершения клиентского контекста.
// Важно: используется отдельный контекст для MTProto‑движка, чтобы дать шанс статусам/очередям
// корректно завершиться до гашения сетевого уровня.
func (r *Runner) Run(waiter *floodwait.Waiter, updmgr *tgupdates.Manager) error {
	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()

	// Запускаем отслеживание сигналов сразу, чтобы Ctrl+C работал во время инициализации
	var shutdownWG sync.WaitGroup

	shutdownWG.Go(func() {
		<-r.mainCtx.Done()
		logger.Debug("Shutdown signal received, stopping runner...")
		r.stopAllServices()
		clientCancel()
	})

	return waiter.Run(clientCtx, func(ctx context.Context) error {
		return r.client.Run(ctx, func(ctx context.Context) error {
			logger.Info("Userbot running...")

			self, loginErr := r.loginSelf(ctx)
			if loginErr != nil {
				return loginErr
			}

			if err := r.initPeersIfNeeded(ctx); err != nil {
				return err
			}

			if err := r.startAllServices(ctx, updmgr, self.ID); err != nil {
				r.stopAllServices()
				return err
			}

			<-ctx.Done()
			shutdownWG.Wait()
			return ctx.Err()
		})
	})
}

func (r *Runner) loginSelf(ctx context.Context) (*tg.User, error) {
	// 2) Готовим интерактивный сценарий
	flow := auth.NewFlow(
		core.TerminalAuthenticator{PhoneNumber: config.Env().PhoneNumber},
		auth.SendCodeOptions{},
	)

	if err := r.client.Auth().IfNecessary(ctx, flow); err != nil {
		return nil, errors.Wrap(err, "auth")
	}

	self, err := r.client.Self(ctx)
	if err != nil {
		return nil, err
	}
	logger.Logger().Info("Logged in as:",
		zap.String("FirstName", self.FirstName),
		zap.String("LastName", self.LastName),
		zap.String("Username", self.Username),
		zap.Int64("ID", self.ID),
	)
	return self, nil
}

func (r *Runner) initPeersIfNeeded(ctx context.Context) error {
	if r.peers == nil {
		return nil
	}

	if err := r.peers.Mgr.Init(ctx); err != nil {
		logger.Errorf("failed to init peers manager: %v", err)
		if config.Env().Notifier == notifierClient {
			return err
		}
	}

	if err := r.peers.LoadFromStorage(ctx); err != nil {
		logger.Errorf("failed to load peers from storage: %v", err)
	}

	if err := r.peers.WarmupIfEmpty(ctx, r.client.API()); err != nil {
		logger.Errorf("failed to warm up peers manager: %v", err)
		if config.Env().Notifier == notifierClient {
			logger.Error("peers warmup error, cant use client notifier")
			return err
		}
	}

	logger.Debug("Peers warmup complete")
	return nil
}

func (r *Runner) startAllServices(ctx context.Context, updmgr *tgupdates.Manager, selfID int64) error {
	// command executor
	logger.Debug("initializing command executor")
	r.cmdExecutor = commands.NewExecutor(r.client, r.filters, r.notif, r.peers)
	logger.Debug("command executor initialized")

	// cli
	logger.Debug("starting service cli")
	r.cliService = cli.NewService(r.cmdExecutor, r.mainCancel)
	r.cliService.Start(ctx)
	logger.Debug("service cli started")

	// web server (если включен)
	if config.Env().WebServerEnable {
		logger.Debug("starting service web_server")
		r.webServer = web.NewServer(r.cmdExecutor)

		// Передаем webServer в handlers для генерации auth токенов
		r.handlers.SetWebAuth(r.webServer)

		go func() {
			if err := r.webServer.Start(); err != nil {
				logger.Errorf("web server error: %v", err)
			}
		}()
		logger.Debug("service web_server started")
	}

	// peers_manager (если есть)
	// peers уже инициализированы в app.Init, ничего не делаем

	// connection_manager
	logger.Debug("starting service connection_manager")
	connection.Init(ctx, r.client)
	logger.Debug("service connection_manager started")

	// status_manager
	logger.Debug("starting service status_manager")
	status.Start(ctx, r.client)
	logger.Debug("service status_manager started")

	// deduplicator
	logger.Debug("starting service deduplicator")
	r.dedup.Start(ctx)
	logger.Debug("service deduplicator started")

	// debouncer
	logger.Debug("starting service debouncer")
	r.deb.Start(ctx)
	logger.Debug("service debouncer started")

	// notifications_queue
	logger.Debug("starting service notifications_queue")
	r.notif.Start(ctx)
	logger.Debug("service notifications_queue started")

	// domain_handlers
	logger.Debug("starting service domain_handlers")
	r.handlers.Start(ctx, CleanPeriodHours*time.Hour)
	logger.Debug("service domain_handlers started")

	// updates_manager
	logger.Debug("starting service updates_manager")
	// Создаем отдельный контекст для updates_manager, который можно отменить независимо
	updatesCtx, updatesCancel := context.WithCancel(ctx)
	r.updatesCancel = updatesCancel
	r.updatesWG.Go(func() {
		logger.Debug("updates_manager service: Run started")
		mgrErr := updmgr.Run(updatesCtx, r.client.API(), selfID, tgupdates.AuthOptions{
			Forget:  false,
			OnStart: r.handleUpdatesManagerStart,
		})
		if mgrErr != nil && !errors.Is(mgrErr, context.Canceled) {
			logger.Errorf("updmgr.Run return: %v", mgrErr)
			r.mainCancel()
		}
		logger.Debugf("updates_manager service: Run finished (err=%v)", mgrErr)
		// По завершении обновлений инициируем общий shutdown.
		// r.stop()
	})
	logger.Debug("service updates_manager started")

	return nil
}

func (r *Runner) stopAllServices() {
	// Останавливаем в обратном порядке

	// updates_manager
	logger.Debug("stopping service updates_manager")
	if r.updatesCancel != nil {
		r.updatesCancel() // Отменяем контекст updates_manager
	}
	r.updatesWG.Wait() // Ждем завершения горутины
	logger.Debug("service updates_manager stopped")

	// status_manager
	logger.Debug("stopping service status_manager")
	status.Stop()
	logger.Debug("service status_manager stopped")

	// domain_handlers
	logger.Debug("stopping service domain_handlers")
	r.handlers.Stop()
	logger.Debug("service domain_handlers stopped")

	// notifications_queue
	logger.Debug("stopping service notifications_queue")
	if err := r.notif.Stop(); err != nil {
		logger.Errorf("stop notifications_queue: %v", err)
	}
	logger.Debug("service notifications_queue stopped")

	// debouncer
	logger.Debug("stopping service debouncer")
	r.deb.Stop()
	logger.Debug("service debouncer stopped")

	// deduplicator
	logger.Debug("stopping service deduplicator")
	r.dedup.Stop()
	logger.Debug("service deduplicator stopped")

	// connection_manager
	logger.Debug("stopping service connection_manager")
	connection.Shutdown()
	logger.Debug("service connection_manager stopped")

	// peers_manager (если есть)
	if r.peers != nil {
		logger.Debug("stopping service peers_manager")
		if err := r.peers.Close(); err != nil {
			logger.Errorf("failed to stop peers_manager: %v", err)
		}
		logger.Debug("service peers_manager stopped")
	}

	// web server
	if r.webServer != nil {
		logger.Debug("stopping service web_server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), webServerShutdownTimeout)
		defer cancel()
		if err := r.webServer.Shutdown(shutdownCtx); err != nil {
			logger.Errorf("failed to stop web_server: %v", err)
		}
		logger.Debug("service web_server stopped")
	}

	// cli
	if r.cliService != nil {
		logger.Debug("stopping service cli")
		r.cliService.Stop()
		logger.Debug("service cli stopped")
	}
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
}
