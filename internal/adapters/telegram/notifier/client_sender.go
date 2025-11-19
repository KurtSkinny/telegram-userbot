// Package telegramnotifier реализует отправку уведомлений через MTProto-клиента.
// Пакет предоставляет PreparedSender, который обеспечивает идемпотентную доставку
// заданий очереди: соблюдает троттлинг, корректно переживает обрывы соединения,
// классифицирует ошибки и поддерживает повторные попытки. В рамках файла
// client_sender.go реализована доставка текста и опциональный ре-форвард
// исходных сообщений с детерминированными random_id, чтобы ретраи не
// создавали дубликатов.
package telegramnotifier

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"telegram-userbot/internal/domain/filters"
	"telegram-userbot/internal/domain/notifications"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/telegram/connection"
	"telegram-userbot/internal/infra/telegram/peersmgr"
	telegramruntime "telegram-userbot/internal/infra/telegram/runtime"
	"telegram-userbot/internal/infra/telegram/status"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// ClientSender выполняет доставку уведомлений через заданный MTProto-клиент.
type ClientSender struct {
	api   *tg.Client
	peers *peersmgr.Service
}

// NewClientSender создаёт ClientSender с заданным api и rps (requests per second).
func NewClientSender(api *tg.Client, rps int, peers *peersmgr.Service) *ClientSender {
	if peers == nil {
		panic("ClientSender: peers manager must not be nil")
	}

	return &ClientSender{
		api:   api,
		peers: peers,
	}
}

// BeforeDrain вызывается очередью один раз перед первой фактической отправкой
// в рамках сессии дренирования (urgent или regular). Выполняет:
//  1. перевод аккаунта в online;
//  2. небольшую случайную паузу для «очеловечивания» активности.
func (s *ClientSender) BeforeDrain(ctx context.Context) {
	// 1) Перейти в онлайн (чтобы MTProto-аккаунт считался активным для собеседников).
	status.GoOnline()

	// 2) Случайная задержка для «очеловечивания» (если у вас такая политика).
	// Неблокирующая отмена через ctx.
	telegramruntime.WaitRandomTime(ctx)
}

// Deliver выполняет одно задание: для получателя в job:
//  1. резолвит peer через локальный кэш;
//  2. ждёт онлайна и имитирует набор текста;
//  3. отправляет текст; при необходимости — пересылает оригиналы;
//  4. классифицирует ошибки: permanent → пропуск адресата, network → выход,
//     прочие → возврат с флагом Retry.
//
// Возвращает SendOutcome и ошибку, если требуется прерывание дренирования.
func (s *ClientSender) Deliver(ctx context.Context, job notifications.Job) (notifications.SendOutcome, error) {
	var outcome notifications.SendOutcome

	// Предвычисляем инварианты на весь job
	hasText := strings.TrimSpace(job.Payload.Text) != ""
	fwd := job.Payload.Forward
	needForward := fwd != nil && fwd.Enabled && len(fwd.MessageIDs) > 0

	recipient := job.Recipient
	peer, errPeer := s.peers.InputPeerByKind(ctx, recipient.Type.String(), int64(recipient.PeerID))
	if errPeer != nil {
		if errors.Is(errPeer, context.Canceled) || errors.Is(errPeer, context.DeadlineExceeded) {
			return outcome, errPeer
		}

		if connection.HandleError(errPeer) {
			outcome.NetworkDown = true
			// Возвращаем nil в качестве ошибки, чтобы очередь обработала это как временный сбой.
			return outcome, nil
		}

		// Если это не сетевая ошибка, считаем ее постоянной проблемой с получателем.
		logger.Errorf("ClientSender: resolve peer %s:%d failed: %v", recipient.Type, recipient.PeerID, errPeer)
		outcome.PermanentFailures = append(outcome.PermanentFailures, recipient)
		outcome.PermanentError = errors.Join(outcome.PermanentError, errPeer)
		return outcome, nil
	}

	// Убеждаемся, что соединение живо, и показываем «typing».
	connection.WaitOnline(ctx)
	status.DoTypingWaitChars(ctx, peer, job.Payload.Text)

	if hasText {
		skip, retErr := s.handleAPIErr(
			s.apiSendMessage(ctx, job, recipient, peer),
			recipient, &outcome,
		)
		if skip {
			return outcome, nil
		}
		if retErr != nil {
			return outcome, retErr
		}
	}

	if needForward {
		skip, retErr := s.handleAPIErr(
			s.apiForwardMessages(ctx, job, recipient, peer),
			recipient, &outcome,
		)

		if skip {
			return outcome, nil
		}
		if retErr != nil {
			return outcome, retErr
		}
	}

	return outcome, nil
}

// handleAPIErr нормализует ошибки API в один из трёх исходов:
//   - skip=true, err=nil   — постоянная ошибка; адресат пропущен;
//   - skip=любой, err!=nil — немедленное завершение Deliver с этой ошибкой;
//   - skip=false, err=nil  — продолжаем текущего адресата.
//
// Понимает ошибки контекста.
func (s *ClientSender) handleAPIErr(
	err error,
	recipient filters.Recipient,
	outcome *notifications.SendOutcome,
) (bool, error) {
	if err == nil {
		return false, nil
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, err
	}

	rpcErr, ok := tgerr.As(err)
	if ok {
		// PEER_FLOOD считается постоянной ошибкой для массовых рассылок.
		if rpcErr.Type == "PEER_FLOOD" || (rpcErr.Code >= 400 && rpcErr.Code < 500) {
			outcome.PermanentFailures = append(outcome.PermanentFailures, recipient)
			outcome.PermanentError = errors.Join(outcome.PermanentError, err)
			return true, nil
		}
	}

	// Сетевые/MTProto-сбои: просим внешний цикл подождать восстановления соединения.
	if connection.HandleError(err) {
		outcome.NetworkDown = true
		return false, err
	}

	outcome.Retry = true
	return false, err
}

// apiSendMessage отправляет подготовленный текст конкретному получателю.
// Использует детерминированный random_id (jobID+recipient), чтобы повторы
// не создавали дубликаты. При активном форварде отключает предпросмотр ссылок
// (NoWebpage=true), чтобы текст и форвард не конфликтовали визуально.
func (s *ClientSender) apiSendMessage(
	ctx context.Context,
	job notifications.Job,
	recipient filters.Recipient,
	peer tg.InputPeerClass,
) error {
	// Детерминированный random_id: одинаков для всех ретраев этой пары (recipient).
	randomID := notifications.RandomIDForMessage(job, recipient)

	req := &tg.MessagesSendMessageRequest{
		Peer:     peer,
		Message:  job.Payload.Text,
		RandomID: randomID,
	}
	if job.Payload.Forward != nil && job.Payload.Forward.Enabled {
		// При включённом форварде убираем превью ссылок в тексте, чтобы избежать «перемешивания» содержимого.
		req.NoWebpage = true
	}

	logger.Debugf(
		"ClientSender: send message job=%d recipient=%d random_id=%d",
		job.ID, recipient.PeerID, randomID,
	)

	_, err := s.api.MessagesSendMessage(ctx, req)
	if err != nil {
		logger.Debugf(
			"ClientSender: send message failed job=%d recipient=%d random_id=%d err=%v",
			job.ID, recipient.PeerID, randomID, err,
		)
	}
	return err
}

// apiForwardMessages повторно пересылает оригинальные сообщения после успешной доставки текста.
// Резолвит fromPeer, генерирует per-message random_id и делает глубокую копию ID,
// чтобы избежать случайной мутации исходного слайса при ретраях.
func (s *ClientSender) apiForwardMessages(
	ctx context.Context,
	job notifications.Job,
	recipient filters.Recipient,
	toPeer tg.InputPeerClass,
) error {
	fwd := job.Payload.Forward
	fromPeer, err := s.peers.InputPeerByKind(ctx, fwd.FromPeer.Type.String(), int64(fwd.FromPeer.PeerID))
	if err != nil {
		return fmt.Errorf("resolve forward peer %s:%d: %w", fwd.FromPeer.Type, fwd.FromPeer.PeerID, err)
	}

	// Для каждого исходного message_id генерируем свой random_id для идемпотентности ретраев.
	randomIDs := notifications.RandomIDsForForward(job, recipient, fwd.FromPeer, fwd.MessageIDs)
	if len(randomIDs) == 0 {
		return nil
	}

	logger.Debugf(
		"ClientSender: forward messages job=%d recipient=%d message_ids=%v random_ids=%v",
		job.ID, recipient.PeerID, fwd.MessageIDs, randomIDs,
	)

	req := &tg.MessagesForwardMessagesRequest{
		// Копируем слайс ID, чтобы избежать aliasing при возможных переиспользованиях job.Payload.
		FromPeer: fromPeer,
		ID:       append([]int(nil), fwd.MessageIDs...),
		ToPeer:   toPeer,
		RandomID: randomIDs,
	}

	_, errFwd := s.api.MessagesForwardMessages(ctx, req)
	if errFwd != nil {
		logger.Debugf(
			"ClientSender: forward failed job=%d recipient=%d message_ids=%v err=%v",
			job.ID, recipient.PeerID, fwd.MessageIDs, errFwd,
		)
	}
	return errFwd
}
