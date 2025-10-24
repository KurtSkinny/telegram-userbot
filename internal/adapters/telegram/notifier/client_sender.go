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

	"telegram-userbot/internal/domain/notifications"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/telegram/cache"
	"telegram-userbot/internal/infra/telegram/connection"
	telegramruntime "telegram-userbot/internal/infra/telegram/runtime"
	"telegram-userbot/internal/infra/telegram/status"
	"telegram-userbot/internal/infra/throttle"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// stopRetryReason используется для высокоуровневой классификации причин
// остановки ретраев. Значение вкладывается в stopRetryError и считывается
// внешней логикой Deliver/троттлером.
type stopRetryReason int

const (
	// stopRetryReasonPermanent — повторять бессмысленно (например, 400-ошибка).
	stopRetryReasonPermanent stopRetryReason = iota
	// stopRetryReasonNetwork — требуется дождаться восстановления соединения.
	stopRetryReasonNetwork
)

// stopRetryError — обёртка над первичной ошибкой, сигнализирующая, что
// ретраи нужно прекратить. Метод StopRetry() позволяет троттлеру распознать
// её без привязки к конкретным типам ошибок Telegram.
type stopRetryError struct {
	err    error
	reason stopRetryReason
}

func (e *stopRetryError) Error() string { return e.err.Error() }

func (e *stopRetryError) Unwrap() error { return e.err }

// StopRetry позволяет throttler'у распознать необходимость выхода из цикла.
func (e *stopRetryError) StopRetry() bool { return true }

// Reason возвращает причину остановки повторов.
func (e *stopRetryError) Reason() stopRetryReason { return e.reason }

// ClientSender реализует notifications.PreparedSender поверх MTProto-клиента.
// Поля:
//   - api — клиент Telegram;
//   - limiter — троттлер запросов (token bucket) с поддержкой FLOOD_WAIT.
type ClientSender struct {
	api     *tg.Client
	limiter *throttle.Throttler
}

// NewClientSender создаёт PreparedSender, оборачивая tg.Client троттлером.
// Параметр rps задаёт целевую среднюю частоту запросов. Подключён
// FloodWaitExtractor для корректной паузы при FLOOD_WAIT/FLOOD_PREMIUM_WAIT.
func NewClientSender(api *tg.Client, rps int) *ClientSender {
	// Троттлер ограничивает RPS и умеет извлекать обязательные паузы из FLOOD_WAIT.
	throttler := throttle.New(
		rps,
		throttle.WithWaitExtractors(FloodWaitExtractor()),
	)

	return &ClientSender{
		api:     api,
		limiter: throttler,
	}
}

// Start привязывает ограничитель скорости к контексту жизненного цикла очереди.
// Без запуска троттлер не будет выдавать токены и все Do() вернутся с ошибкой ожидания.
func (s *ClientSender) Start(ctx context.Context) {
	if s.limiter != nil {
		s.limiter.Start(ctx)
	}
}

// Stop завершает фоновые горутины токен-бакета и освобождает ресурсы троттлера.
func (s *ClientSender) Stop() {
	if s.limiter != nil {
		s.limiter.Stop()
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

// Deliver выполняет одно задание: для каждого получателя по FIFO:
//  1. резолвит peer через локальный кэш;
//  2. ждёт онлайна и имитирует набор текста;
//  3. отправляет текст; при необходимости — пересылает оригиналы;
//  4. классифицирует ошибки: permanent → пропуск адресата, network → выход,
//     прочие → возврат с флагом Retry.
//
// Возвращает агрегированный SendOutcome и ошибку, если требуется прерывание дренирования.
func (s *ClientSender) Deliver(ctx context.Context, job notifications.Job) (notifications.SendOutcome, error) {
	var outcome notifications.SendOutcome

	// Предвычисляем инварианты на весь job
	hasText := strings.TrimSpace(job.Payload.Text) != ""
	fwd := job.Payload.Forward
	needForward := fwd != nil && fwd.Enabled && len(fwd.MessageIDs) > 0

	for idx, recipient := range job.Recipients {
		peer, errPeer := cache.GetInputPeerByKind(recipient.Type, recipient.ID)
		if errPeer != nil {
			logger.Errorf("ClientSender: resolve peer %s:%d failed: %v", recipient.Type, recipient.ID, errPeer)
			outcome.PermanentFailures = append(outcome.PermanentFailures, recipient)
			outcome.PermanentError = errors.Join(outcome.PermanentError, errPeer)
			continue
		}

		// Перед каждым адресатом убеждаемся, что соединение живо, и показываем «typing».
		connection.WaitOnline(ctx)
		status.DoTypingWaitChars(ctx, peer, job.Payload.Text)

		if hasText {
			skip, retErr := s.handleAPIErr(
				s.apiSendMessage(ctx, job, recipient, idx, peer),
				recipient, &outcome,
			)
			if skip {
				continue
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
				continue
			}
			if retErr != nil {
				return outcome, retErr
			}
		}
	}

	return outcome, nil
}

// handleAPIErr нормализует ошибки API в один из трёх исходов:
//   - skip=true, err=nil   — постоянная ошибка; адресат пропущен;
//   - skip=любой, err!=nil — немедленное завершение Deliver с этой ошибкой;
//   - skip=false, err=nil  — продолжаем текущего адресата.
//
// Понимает stopRetryError и ошибки контекста.
func (s *ClientSender) handleAPIErr(
	err error,
	recipient notifications.Recipient,
	outcome *notifications.SendOutcome,
) (bool, error) {
	if err == nil {
		return false, nil
	}

	if stop, underlying, ok := extractStopReason(err); ok {
		switch stop {
		case stopRetryReasonNetwork:
			outcome.NetworkDown = true
			return false, underlying
		case stopRetryReasonPermanent:
			outcome.PermanentFailures = append(outcome.PermanentFailures, recipient)
			outcome.PermanentError = errors.Join(outcome.PermanentError, underlying)
			return true, nil
		}
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, err
	}

	outcome.Retry = true
	return false, err
}

// apiSendMessage отправляет подготовленный текст конкретному получателю.
// Использует детерминированный random_id (jobID+recipient+index), чтобы повторы
// не создавали дубликаты. При активном форварде отключает предпросмотр ссылок
// (NoWebpage=true), чтобы текст и форвард не конфликтовали визуально.
func (s *ClientSender) apiSendMessage(
	ctx context.Context,
	job notifications.Job,
	recipient notifications.Recipient,
	index int,
	peer tg.InputPeerClass,
) error {
	// Детерминированный random_id: одинаков для всех ретраев этой пары (recipient,index).
	randomID := notifications.RandomIDForMessage(job.ID, recipient, index)

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
		job.ID, recipient.ID, randomID,
	)

	return s.limiter.Do(ctx, func() error {
		_, err := s.api.MessagesSendMessage(ctx, req)
		if err == nil {
			return nil
		}

		logger.Debugf(
			"ClientSender: send message failed job=%d recipient=%d random_id=%d err=%v",
			job.ID, recipient.ID, randomID, err,
		)

		// Сетевые/MTProto-сбои: просим внешний цикл подождать восстановления соединения.
		if connection.HandleError(err) {
			return &stopRetryError{err: err, reason: stopRetryReasonNetwork}
		}
		// Постоянные ошибки (4xx/PEER_FLOOD и т.п.): ретраить бессмысленно, пометим адресата.
		if isPermanentRPCError(err) {
			return &stopRetryError{err: err, reason: stopRetryReasonPermanent}
		}
		return err
	})
}

// apiForwardMessages повторно пересылает оригинальные сообщения после успешной доставки текста.
// Резолвит fromPeer, генерирует per-message random_id и делает глубокую копию ID,
// чтобы избежать случайной мутации исходного слайса при ретраях.
func (s *ClientSender) apiForwardMessages(
	ctx context.Context,
	job notifications.Job,
	recipient notifications.Recipient,
	toPeer tg.InputPeerClass,
) error {
	fwd := job.Payload.Forward
	fromPeer, err := cache.GetInputPeerByKind(fwd.FromPeer.Type, fwd.FromPeer.ID)
	if err != nil {
		return &stopRetryError{
			err:    fmt.Errorf("resolve forward peer %s:%d: %w", fwd.FromPeer.Type, fwd.FromPeer.ID, err),
			reason: stopRetryReasonPermanent,
		}
	}

	// Для каждого исходного message_id генерируем свой random_id для идемпотентности ретраев.
	randomIDs := notifications.RandomIDsForForward(job.ID, recipient, fwd.FromPeer, fwd.MessageIDs)
	if len(randomIDs) == 0 {
		return nil
	}

	logger.Debugf(
		"ClientSender: forward messages job=%d recipient=%d message_ids=%v random_ids=%v",
		job.ID, recipient.ID, fwd.MessageIDs, randomIDs,
	)

	req := &tg.MessagesForwardMessagesRequest{
		// Копируем слайс ID, чтобы избежать aliasing при возможных переиспользованиях job.Payload.
		FromPeer: fromPeer,
		ID:       append([]int(nil), fwd.MessageIDs...),
		ToPeer:   toPeer,
		RandomID: randomIDs,
	}

	return s.limiter.Do(ctx, func() error {
		_, errFwd := s.api.MessagesForwardMessages(ctx, req)
		if errFwd == nil {
			return nil
		}

		logger.Debugf(
			"ClientSender: forward failed job=%d recipient=%d message_ids=%v err=%v",
			job.ID, recipient.ID, fwd.MessageIDs, errFwd,
		)

		// Сетевые/MTProto-сбои при форварде: просим подождать восстановление.
		if connection.HandleError(errFwd) {
			return &stopRetryError{err: errFwd, reason: stopRetryReasonNetwork}
		}
		// Постоянная RPC-ошибка — пропускаем адресата, повторять нет смысла.
		if isPermanentRPCError(errFwd) {
			return &stopRetryError{err: errFwd, reason: stopRetryReasonPermanent}
		}
		return errFwd
	})
}

// extractStopReason извлекает stopRetryError и возвращает (reason, underlying, true).
// Если err не является stopRetryError, возвращает (0, err, false).
func extractStopReason(err error) (stopRetryReason, error, bool) {
	var stopErr *stopRetryError
	if errors.As(err, &stopErr) {
		return stopErr.reason, stopErr.err, true
	}
	return 0, err, false
}

// isPermanentRPCError классифицирует RPC-ошибки Telegram: FLOOD_WAIT — временная;
// PEER_FLOOD и прочие 4xx считаются постоянными для нашей модели доставки.
func isPermanentRPCError(err error) bool {
	rpcErr, ok := tgerr.As(err)
	if !ok {
		return false
	}
	// FLOOD_WAIT относится к временным ошибкам.
	if rpcErr.Type == "FLOOD_WAIT" {
		return false
	}
	// PEER_FLOOD считается постоянной ошибкой для массовых рассылок.
	if rpcErr.Type == "PEER_FLOOD" {
		return true
	}
	if rpcErr.Code >= 400 && rpcErr.Code < 500 {
		return true
	}
	return false
}

// func (s *ClientSender) DeliverOld(ctx context.Context, job notifications.Job) (notifications.SendOutcome, error) {
// 	var outcome notifications.SendOutcome

// 	for idx, recipient := range job.Recipients {
// 		peer, errPeer := cache.GetInputPeerByKind(recipient.Type, recipient.ID)
// 		if errPeer != nil {
// 			logger.Errorf("ClientSender: resolve peer %s:%d failed: %v", recipient.Type, recipient.ID, errPeer)
// 			outcome.PermanentFailures = append(outcome.PermanentFailures, recipient)
// 			outcome.PermanentError = errors.Join(outcome.PermanentError, errPeer)
// 			continue
// 		}

// 		if strings.TrimSpace(job.Payload.Text) != "" {
// 			if err := s.apiSendMessage(ctx, job, recipient, idx, peer); err != nil {
// 				if stop, underlying, ok := extractStopReason(err); ok {
// 					switch stop {
// 					case stopRetryReasonNetwork:
// 						outcome.NetworkDown = true
// 						return outcome, underlying
// 					case stopRetryReasonPermanent:
// 						outcome.PermanentFailures = append(outcome.PermanentFailures, recipient)
// 						outcome.PermanentError = errors.Join(outcome.PermanentError, underlying)
// 						continue
// 					}
// 				}
// 				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
// 					return outcome, err
// 				}
// 				outcome.Retry = true
// 				return outcome, err
// 			}
// 		}

// 		fwd := job.Payload.Forward
// 		if fwd != nil && fwd.Enabled && len(fwd.MessageIDs) > 0 {
// 			if err := s.apiForwardMessages(ctx, job, recipient, peer); err != nil {
// 				if stop, underlying, ok := extractStopReason(err); ok {
// 					switch stop {
// 					case stopRetryReasonNetwork:
// 						outcome.NetworkDown = true
// 						return outcome, underlying
// 					case stopRetryReasonPermanent:
// 						outcome.PermanentFailures = append(outcome.PermanentFailures, recipient)
// 						outcome.PermanentError = errors.Join(outcome.PermanentError, underlying)
// 						continue
// 					}
// 				}
// 				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
// 					return outcome, err
// 				}
// 				outcome.Retry = true
// 				return outcome, err
// 			}
// 		}
// 	}

// 	return outcome, nil
// }
