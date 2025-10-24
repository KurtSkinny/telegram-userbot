// Package updates — подсистема обработки апдейтов, отвечающая в этом файле за
// «человеческую» синхронизацию прочитанного. Здесь реализованы выборка
// накопленных непрочитанных сообщений по пирами и их пометка как прочитанных с
// реалистичными задержками. Ключевые идеи:
//   - имитация пользовательского поведения через случайные интервалы ожидания,
//   - отсутствие сетевых вызовов под мьютексом,
//   - разрешение inputPeer через локальный кэш с best‑effort стратегией,
//   - уважение белого списка диалогов (config.UniqueChats), чтобы не трогать лишнее.

package updates

import (
	"context"
	"math/rand/v2"
	"slices"
	"time"

	"telegram-userbot/internal/domain/filters"
	"telegram-userbot/internal/infra/config"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/telegram/cache"
	"telegram-userbot/internal/infra/telegram/connection"
	tgruntime "telegram-userbot/internal/infra/telegram/runtime"
	"telegram-userbot/internal/infra/telegram/status"

	"github.com/gotd/td/tg"
)

// Константы таймингов. Значения подбирались для того, чтобы выглядеть «неботообразно»:
//   - readDelayMinMs/readDelayMaxMs — паузы между отметками прочитанного в разных чатах;
//   - runSchedulerWt — базовый интервал запуска фонового прохода (минуты);
//   - runSchedulerWtJitter — джиттер к базовому интервалу (минуты).
const (
	readDelayMinMs       = 5000
	readDelayMaxMs       = 10000
	runSchedulerWt       = 10 // минуты (def 10)
	runSchedulerWtJitter = 30 // минуты (def 30)
)

// runMarkReadScheduler запускает фонового воркера, который с псевдослучайным
// интервалом просматривает локальный кэш непрочитанных (h.unread) и инициирует
// их пометку как прочитанных. Важные детали реализации:
//   - период ожидания выбирается как min + rand(jitter), что размывает паттерн;
//   - перед Reset таймера корректно сбрасываем его состояние и дреним канал;
//   - по ctx.Done() выходим мягко, без зависаний на таймере.
func (h *Handlers) runMarkReadScheduler(ctx context.Context) {
	minWt := runSchedulerWt
	jitterWt := runSchedulerWtJitter

	timer := time.NewTimer(0)
	defer timer.Stop()
	logger.Debug("StartMarkReadScheduler started.")
	for {
		// Нужен именно «человеческий» джиттер, а не криптографический — поэтому
		// используем math/rand. Линтер G404 шумит, отметка #nosec осознанна.
		delay := time.Duration(minWt+rand.IntN(jitterWt)) * time.Minute // #nosec G404

		// Перед Reset обязательно останавливаем таймер и, если тик уже случился,
		// вычитываем из канала, чтобы не получить немедленный спурионный тик.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(delay)

		select {
		case <-ctx.Done():
			logger.Debug("StartMarkReadScheduler stopped.")
			return
		case <-timer.C:
			h.flushUnread(ctx)
		}
	}
}

// flushUnread достаёт накопленные «непрочитанные» из h.unread под мьютексом,
// пытается разрешить для них inputPeer и, уже без блокировки, последовательно
// отмечает чаты как прочитанные, соблюдая случайные задержки между вызовами.
// Ошибки разрешения пиров логируются и не блокируют обработку остальных.
func (h *Handlers) flushUnread(ctx context.Context) {
	h.unreadMu.Lock()
	// Собираем срез сообщений-кандидатов и общий Entities-контейнер для cache.GetInputPeerRaw.
	entities := tg.Entities{}
	messages := []*tg.Message{}

	for peerID, maxID := range h.unread {
		// Уважаем белый список: если чат не разрешён конфигурацией, очищаем запись и пропускаем.
		if !slices.Contains(config.UniqueChats(), peerID) {
			delete(h.unread, peerID)
			continue
		}

		msg := &tg.Message{ID: maxID}

		// Пытаемся угадать тип peer: канал, чат или пользователь — в таком порядке.
		candidates := []tg.PeerClass{
			&tg.PeerChannel{ChannelID: peerID},
			&tg.PeerChat{ChatID: peerID},
			&tg.PeerUser{UserID: peerID},
		}

		var (
			resolved bool
			lastErr  error
		)

		for _, candidate := range candidates {
			msg.PeerID = candidate
			// Best‑effort разрешение через локальный кэш: на успех достаточно любой подходящей формы peer.
			if _, err := cache.GetInputPeerRaw(entities, msg); err == nil {
				resolved = true
				break
			} else {
				lastErr = err
			}
		}

		if resolved {
			messages = append(messages, msg)
		} else {
			logger.Errorf("markRead: failed to resolve peer %d: %v", peerID, lastErr)
		}
	}
	h.unreadMu.Unlock()
	// Дальше только сетевые операции — выполняем их без удержания мьютекса.

	// logger.Warn(pr.Pf(messages))
	if len(messages) == 0 {
		return
	}

	// Перед серией сетевых вызовов убеждаемся, что клиент онлайн, и кратко «засветим» активность.
	connection.WaitOnline(ctx)
	status.GoOnline()
	// Небольшая задержка перед первым запросом, чтобы не выглядеть как бот-триггер.
	tgruntime.WaitRandomTimeMs(ctx, readDelayMinMs, readDelayMaxMs)

	for i, msg := range messages {
		// Помечаем диалог как прочитанный до msg.ID. Между чатами — пауза.
		h.markRead(ctx, entities, msg)
		if i < len(messages)-1 {
			tgruntime.WaitRandomTimeMs(ctx, readDelayMinMs, readDelayMaxMs)
		}
	}
	// Хвостовая пауза, чтобы статус «онлайн» не состоялся сразу после последнего запроса.
	tgruntime.WaitRandomTimeMs(ctx, readDelayMinMs, readDelayMaxMs)

	logger.Debug("markRead complited")
}

// markRead разрешает InputPeer и отправляет соответствующий read‑запрос:
//   - для пользователей/групп — MessagesReadHistory,
//   - для каналов — ChannelsReadHistory.
//
// Перед каждым вызовом дожидается онлайна, ошибки пробрасывает в connection.HandleError
// и при успехе синхронизирует локальный кэш lastUnreadCache, чтобы не повторять работу.
func (h *Handlers) markRead(ctx context.Context, entities tg.Entities, msg *tg.Message) {
	peer, pErr := cache.GetInputPeerRaw(entities, msg)
	if pErr != nil {
		logger.Errorf("markRead: get input peer failed: %v", pErr.Error())
		return
	}

	peerID := filters.GetPeerID(msg.PeerID)

	switch p := peer.(type) {
	case *tg.InputPeerUser, *tg.InputPeerChat:
		connection.WaitOnline(ctx)
		if _, err := h.api.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{
			Peer:  p,
			MaxID: msg.ID,
		}); err != nil {
			connection.HandleError(err)
			logger.Errorf("markRead: Messages.readHistory failed: %v", err.Error())
		} else {
			h.lastUnreadCache(peerID, msg.ID)
		}
	case *tg.InputPeerChannel:
		ch := &tg.InputChannel{
			ChannelID:  p.ChannelID,
			AccessHash: p.AccessHash,
		}
		connection.WaitOnline(ctx)
		if _, err := h.api.ChannelsReadHistory(ctx, &tg.ChannelsReadHistoryRequest{
			Channel: ch,
			MaxID:   msg.ID,
		}); err != nil {
			connection.HandleError(err)
			logger.Errorf("markRead: channels.readHistory failed: %v", err.Error())
		} else {
			h.lastUnreadCache(peerID, msg.ID)
		}
	default:
		logger.Errorf("markRead: unsupported peer type %T", p)
	}
}

// setUnreadCache атомарно обновляет для peerID максимальный msgID, который нужно
// «дочитать». Предполагается монотонный рост идентификаторов; меньшие значения игнорируются.
func (h *Handlers) setUnreadCache(peerID int64, msgID int) {
	h.unreadMu.Lock()
	if msgID > h.unread[peerID] {
		h.unread[peerID] = msgID
	}
	h.unreadMu.Unlock()
}

// lastUnreadCache очищает запись о непрочитанном для peerID, если успешно
// дочитали именно до зафиксированной ранее границы. Это защищает от гонок,
// когда параллельная ветка могла обновить h.unread до большего значения.
func (h *Handlers) lastUnreadCache(peerID int64, msgID int) {
	h.unreadMu.Lock()
	if val, ok := h.unread[peerID]; ok {
		if msgID == val {
			delete(h.unread, peerID)
		}
	}
	h.unreadMu.Unlock()
}
