// Package updates содержит обработчики входящих событий Telegram и связывает
// транспортный слой (tg.* updates) с бизнес-логикой уведомлений. В рамках
// пакета решаются задачи:
//  1. фильтрация сообщений по заданным правилам (internal/domain/filters),
//  2. идемпотентная доставка уведомлений (notified-кэш + очередь уведомлений),
//  3. защита от повторной обработки (Deduplicator по peerID/msgID/EditDate),
//  4. сглаживание всплесков при частых правках одного сообщения (Debouncer),
//  5. поддержание локальных счетчиков непрочитанного для эвристик.
//
// Пакет не отправляет сообщения сам по себе — он формирует и ставит задачи в
// очередь уведомлений, а также ведет кэш «что уже уведомляли», чтобы не
// дублировать рассылку при редактированиях и повторных апдейтах от Telegram.
package updates

import (
	"context"
	"sync"
	"time"

	"telegram-userbot/internal/domain/filters"
	"telegram-userbot/internal/domain/notifications"
	"telegram-userbot/internal/domain/tgutil"
	"telegram-userbot/internal/infra/concurrency"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/telegram/cache"
	"telegram-userbot/internal/support/debug"

	"telegram-userbot/internal/infra/config"

	"github.com/gotd/td/tg"
)

// Handlers агрегирует зависимости и реализует реакции на ключевые типы
// апдейтов Telegram. Экземпляр поддерживает:
//   - обращение к Telegram API для служебных операций;
//   - постановку уведомлений в очередь с соблюдением идемпотентности;
//   - локальный кэш комбинаций «сообщение × фильтр», уже отработанных;
//   - дедупликацию по (peerID, msgID, editDate), чтобы не переобрабатывать
//     одно и то же содержимое;
//   - дебаунс частых правок одного сообщения, чтобы не заспамить очередь;
//   - грубые счетчики непрочитанного по пирам для вспомогательных эвристик;
//   - фоновую очистку устаревших отметок notified и периодический сброс их на диск.
type Handlers struct {
	api       *tg.Client                // api предоставляет доступ к TDLib-клиенту для служебных запросов
	notif     *notifications.Queue      // notif отвечает за доставку уведомлений конечному пользователю
	notified  map[string]time.Time      // notified запоминает, какие комбинации «сообщение-фильтр» уже уведомлялись
	mu        sync.Mutex                // mu защищает доступ к карте notified в конкурентной среде
	dupCache  *concurrency.Deduplicator // dupCache предотвращает повторную обработку одинаковых сообщений
	debouncer *concurrency.Debouncer    // debouncer сглаживает частые обновления одного сообщения (редактирования)
	unread    map[int64]int             // unread хранит счётчики непрочитанных сообщений по пирами
	unreadMu  sync.Mutex                // unreadMu синхронизирует конкурентные обновления карты unread

	notifiedCacheFile string
	notifiedDirty     bool
	notifiedSaveTimer *time.Timer

	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	cleanTTL  time.Duration

	shutdown context.CancelFunc
}

// NewHandlers подготавливает инстанс обработчиков: связывает Telegram-клиента,
// очередь уведомлений и утилиты конкурентного доступа (дедупликатор,
// дебаунсер). Значимые параметры берутся из конфигурации окружения:
//   - NotifiedTTLDays — срок хранения отметок «уже уведомлено»;
//   - NotifiedCacheFile — путь файла для периодического флаша notified-кэша.
//
// Возвращает полностью инициализированную структуру без запуска фоновых горутин.
func NewHandlers(api *tg.Client, notif *notifications.Queue,
	dup *concurrency.Deduplicator, debouncer *concurrency.Debouncer,
	shutdown func()) *Handlers {
	cfg := config.Env()
	return &Handlers{
		api:               api,
		notif:             notif,
		notified:          make(map[string]time.Time),
		dupCache:          dup,
		debouncer:         debouncer,
		unread:            make(map[int64]int),
		cleanTTL:          time.Duration(cfg.NotifiedTTLDays) * 24 * time.Hour,
		shutdown:          shutdown,
		notifiedCacheFile: cfg.NotifiedCacheFile,
	}
}

// Start запускает фоновые воркеры и восстанавливает состояние notified-кэша.
// Последовательность:
//  1. опционально переопределяет TTL очистки, если передан аргумент > 0;
//  2. загружает кэш notified с диска (best-effort, ошибки не критичны);
//  3. поднимает контекст отмены и стартует:
//     - планировщик отметок прочитанного (runMarkReadScheduler),
//     - сборщик мусора для notified (runNotificationCacheCleaner).
//
// Повторные вызовы безопасны и игнорируются (startOnce).
func (h *Handlers) Start(ctx context.Context, cleanTTL time.Duration) {
	if ctx == nil {
		return
	}

	h.startOnce.Do(func() {
		if cleanTTL > 0 {
			h.cleanTTL = cleanTTL
		}
		// загрузка кэша notified с диска
		h.loadNotifiedFromDisk()

		runCtx, cancel := context.WithCancel(ctx)
		h.cancel = cancel

		h.wg.Go(func() {
			h.runMarkReadScheduler(runCtx)
		})

		h.wg.Go(func() {
			h.runNotificationCacheCleaner(runCtx)
		})
	})
}

// Stop корректно останавливает запущенные воркеры: вызывает cancel контекста,
// дожидается завершения горутин и делает принудительный флаш notified-кэша на
// диск. Повторные вызовы безопасны и игнорируются (stopOnce).
func (h *Handlers) Stop() {
	h.stopOnce.Do(func() {
		if h.cancel != nil {
			h.cancel()
		}
		h.wg.Wait()
		// Финальный флаш кэша notified на диск
		h.flushNotifiedNow()
	})
}

// OnNewMessage обрабатывает входящее личное или групповое сообщение.
// Пайплайн:
//  1. отбрасывает исходящие сообщения (msg.Out);
//  2. прогревает кэш inputPeer по entities;
//  3. делает быструю дедупликацию по (peerID, msgID, editDate);
//  4. обрабатывает служебную команду "Exit" для завершения процесса;
//  5. прогоняет текст через filters.ProcessMessage и для каждого совпадения
//     проверяет идемпотентность (hasNotified);
//  6. ставит задачу уведомления в очередь и помечает пару (msg, filterID)
//     как доставленную, чтобы избежать повторов при редактированиях;
//  7. обновляет локальные счётчики непрочитанного.
//
// Возвращает ошибку только в случае сбоя постановки уведомления.
func (h *Handlers) OnNewMessage(
	ctx context.Context,
	entities tg.Entities,
	u *tg.UpdateNewMessage,
) error {
	msg, ok := u.Message.(*tg.Message)
	if !ok || msg.Out {
		return nil
	}

	peerID := tgutil.GetPeerID(msg.PeerID)

	// Подтягиваем и прогреваем кэш соответствий peer -> inputPeer.
	// Ошибки намеренно игнорируются: отсутствие записи не критично, а функция
	// сама добавит недостающие сущности в кэш на будущее.
	_, _ = cache.GetInputPeerRaw(entities, msg)

	// Быстрая защита от повторной обработки: та же комбинация
	// (peerID, msgID, editDate) уже приходила и была обработана.
	if h.dupCache.DedupSeen(peerID, msg.ID, msg.EditDate) {
		return nil
	}

	// Сервисный триггер: входящее сообщение с текстом "Exit" инициирует graceful shutdown.
	if msg.Message == "Exit" {
		logger.Info("Shutdown requested via incoming message")
		// if h.cancel != nil {
		// 	h.cancel()
		// }
		if h.shutdown != nil {
			h.shutdown()
		}
		return nil
	}

	logger.Debug("OnNewMessage")
	debug.PrintUpdate("DM/Group", msg, entities)
	results := filters.ProcessMessage(entities, msg)
	for _, res := range results {
		if h.hasNotified(msg, res.Filter.ID) {
			continue
		}
		if err := h.notif.Notify(ctx, entities, msg, res); err != nil {
			// Ошибка здесь — редкая валидационная (nil msg / пустые получатели). Не помечаем.
			logger.Errorf("notify enqueue error: %v", err)
			continue
		}
		h.markNotified(msg, res.Filter.ID)
	}
	// Обновляем локальный счётчик "непрочитанных" для дальнейших эвристик.
	h.setUnreadCache(peerID, msg.ID)
	return nil
}

// OnNewChannelMessage обрабатывает входящее сообщение из канала. Логика
// идентична личным/групповым сообщениям: прогрев кэша, дедупликация,
// фильтрация, идемпотентная постановка уведомлений и обновление счётчиков.
func (h *Handlers) OnNewChannelMessage(
	ctx context.Context,
	entities tg.Entities,
	u *tg.UpdateNewChannelMessage,
) error {
	msg, ok := u.Message.(*tg.Message)
	if !ok || msg.Out {
		return nil
	}

	peerID := tgutil.GetPeerID(msg.PeerID)

	// Подтягиваем и прогреваем кэш соответствий peer -> inputPeer.
	// Ошибки намеренно игнорируются: отсутствие записи не критично, а функция
	// сама добавит недостающие сущности в кэш на будущее.
	_, _ = cache.GetInputPeerRaw(entities, msg)

	// Дедупликация по (peerID, msgID, editDate) для каналов.
	if h.dupCache.DedupSeen(peerID, msg.ID, msg.EditDate) {
		return nil
	}
	logger.Debug("OnNewChannelMessage")
	debug.PrintUpdate("Channel", msg, entities)
	results := filters.ProcessMessage(entities, msg)
	for _, res := range results {
		if h.hasNotified(msg, res.Filter.ID) {
			continue
		}
		if err := h.notif.Notify(ctx, entities, msg, res); err != nil {
			// Ошибка здесь — редкая валидационная (nil msg / пустые получатели). Не помечаем.
			logger.Errorf("notify enqueue error: %v", err)
			continue
		}
		h.markNotified(msg, res.Filter.ID)
	}
	h.setUnreadCache(peerID, msg.ID)
	return nil
}

// OnEditMessage реагирует на редактирование личных/групповых сообщений.
// Использует Debouncer для сглаживания частых правок и Deduplicator по
// комбинации (peerID, msgID, editDate), чтобы повторно не обрабатывать
// идентичное содержимое. При появлении новых совпадений фильтров ставит
// уведомления и фиксирует отметку notified для пары (msg, filterID).
func (h *Handlers) OnEditMessage(
	ctx context.Context,
	entities tg.Entities,
	u *tg.UpdateEditMessage,
) error {
	msg, ok := u.Message.(*tg.Message)
	if !ok || msg.Out {
		return nil
	}
	// logger.Warnf("msg: %v", pr.Pf(msg))
	logger.Debug("OnEditMessage")
	debug.PrintUpdate("OnEditMessage", msg, entities)
	// Дебаунсим лавину апдейтов при частых правках одного и того же сообщения.
	h.debouncer.Do(msg.ID, func() {
		if !h.dupCache.DedupSeen(tgutil.GetPeerID(msg.PeerID), msg.ID, msg.EditDate) {
			results := filters.ProcessMessage(entities, msg)
			for _, res := range results {
				if h.hasNotified(msg, res.Filter.ID) {
					continue
				}
				if err := h.notif.Notify(ctx, entities, msg, res); err != nil {
					logger.Errorf("notify enqueue error: %v", err)
					continue
				}
				h.markNotified(msg, res.Filter.ID)
			}
		}
	})
	return nil
}

// OnEditChannelMessage обрабатывает редактирование сообщений в каналах.
// Логика: дебаунс правок, проверка на дедупликацию, повторный прогон через
// фильтры и, при наличии новых матчей, постановка уведомлений с отметкой notified.
func (h *Handlers) OnEditChannelMessage(
	ctx context.Context,
	entities tg.Entities,
	u *tg.UpdateEditChannelMessage,
) error {
	msg, ok := u.Message.(*tg.Message)
	if !ok || msg.Out {
		return nil
	}
	logger.Debug("OnEditChannelMessage")
	debug.PrintUpdate("OnEditChannelMessage", msg, entities)
	// Дебаунсим частые правки сообщений канала, чтобы не заспамить очередь.
	h.debouncer.Do(msg.ID, func() {
		if !h.dupCache.DedupSeen(tgutil.GetPeerID(msg.PeerID), msg.ID, msg.EditDate) {
			results := filters.ProcessMessage(entities, msg)
			for _, res := range results {
				if h.hasNotified(msg, res.Filter.ID) {
					continue
				}
				if err := h.notif.Notify(ctx, entities, msg, res); err != nil {
					logger.Errorf("notify enqueue error: %v", err)
					continue
				}
				h.markNotified(msg, res.Filter.ID)
			}
		}
	})
	return nil
}
