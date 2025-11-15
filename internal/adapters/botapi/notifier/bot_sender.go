// Package botapionotifier предоставляет реализацию PreparedSender на основе Telegram Bot API.
//
// В этом файле (bot_sender.go):
//   - настраивается HTTP‑клиент и общий троттлер запросов;
//   - реализуется последовательная доставка текста и, при необходимости, «копии» исходного сообщения;
//   - классифицируются ошибки Bot API на временные (retry_after) и постоянные (большинство 4xx);
//   - аккуратно извлекается retry_after из заголовков/тела и передается троттлеру через интерфейс.
//
// Бизнес‑логика совпадает с MTProto‑сендером: сначала обычный текст уведомления, затем копия,
// никаких форвардов от имени бота. Random_id не используется, идемпотентность обеспечивается
// повторным вызовом с теми же параметрами и корректным backoff.

package botapionotifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"telegram-userbot/internal/domain/notifications"
)

// httpClientTimeout — таймаут HTTP‑клиента, секунды. Должен покрывать сетевые
// колебания и не зависать бесконечно на медленных соединениях.
const httpClientTimeout = 30

// botSuperPrefix используется для построения chat_id каналов/супергрупп в Bot API.
// Формула: chat_id = -100<channel_id>. Для обычных групп — просто отрицательный id.
const botSuperPrefix int64 = -1000000000000

// BotSender реализует notifications.PreparedSender поверх Telegram Bot API.
//
// Поля:
//   - baseURL — конечная точка sendMessage для заданного бота (с учётом /test);
//   - client  — HTTP‑клиент с умеренным таймаутом;
//   - limiter — общий троттлер (token bucket).
type BotSender struct {
	baseURL string
	client  *http.Client
	limiter *rate.Limiter
}

// NewBotSender создаёт PreparedSender для бота.
//
// Поведение:
//   - при testDC=true добавляет суффикс /test к токену согласно Bot API;
//   - формирует базовый URL вида https://api.telegram.org/bot<token>/sendMessage;
//   - rps задаёт целевую среднюю частоту запросов.
func NewBotSender(token string, testDC bool, rps int) *BotSender {
	if testDC {
		token += "/test"
	}
	base := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)

	return &BotSender{
		baseURL: base,
		client: &http.Client{
			Timeout: httpClientTimeout * time.Second,
		},
		limiter: rate.NewLimiter(rate.Limit(rps), rps),
	}
}



// toBotChatID конвертирует доменного получателя в корректный chat_id для Bot API.
// Пользователь → положительный id. Basic group → отрицательный id.
// Канал/супергруппа → -100<id>. Функция не валидирует существование чата.
func toBotChatID(r notifications.Recipient) int64 {
	switch r.Type {
	case notifications.RecipientTypeUser:
		if r.ID < 0 {
			return -r.ID
		}
		return r.ID
	case notifications.RecipientTypeChat:
		if r.ID > 0 {
			return -r.ID
		}
		return r.ID
	case notifications.RecipientTypeChannel:
		if r.ID > 0 {
			return botSuperPrefix - r.ID
		}
		return r.ID
	default:
		return r.ID
	}
}

// Deliver отправляет уведомление одному получателю (job.Recipient).
// Если в payload включён Forward и подготовлена копия исходного сообщения (text+entities),
// бот отправляет ДВА сообщения: (1) обычный текст уведомления и (2) копию исходного текста
// с entities (не форвард). Такая последовательность совпадает с бизнес‑логикой клиента.
// Возвращает aggregated outcome: Retry=true — нужна повторная попытка позже;
// PermanentFailures — список чатов, для которых Bot API вернул постоянную 4xx‑ошибку.
func (s *BotSender) Deliver(ctx context.Context, job notifications.Job) (notifications.SendOutcome, error) {
	var outcome notifications.SendOutcome

	// Предварительно вычисляем, что именно будем отправлять: обычный текст и/или «копию».
	hasText := strings.TrimSpace(job.Payload.Text) != ""
	hasCopy := job.Payload.Forward != nil &&
		job.Payload.Forward.Enabled &&
		job.Payload.Copy != nil &&
		strings.TrimSpace(job.Payload.Copy.Text) != ""

	recipient := job.Recipient
	// 1) Сначала отправляем обычный текст уведомления, если он есть.
	if hasText {
		chatID := toBotChatID(recipient)
		permanent, err := s.sendMessage(ctx, chatID, job.Payload.Text)
		if err != nil {
			if permanent {
				outcome.PermanentFailures = append(outcome.PermanentFailures, recipient)
				outcome.PermanentError = errors.Join(outcome.PermanentError, err)
				return outcome, nil
			}
			// Временная ошибка — прерываем обработку всего job на ретрай.
			outcome.Retry = true
			return outcome, err
		}
	}

	// 2) Затем, если включён forward и подготовлена копия — отправляем копию исходного сообщения.
	if hasCopy {
		chatID := toBotChatID(recipient)
		permanent, err := s.sendMessageRich(ctx, chatID, job.Payload.Copy.Text, job.Payload.Copy.Entities)
		if err != nil {
			if permanent {
				outcome.PermanentFailures = append(outcome.PermanentFailures, recipient)
				outcome.PermanentError = errors.Join(outcome.PermanentError, err)
				return outcome, nil
			}
			outcome.Retry = true
			return outcome, err
		}
	}

	return outcome, nil
}

// sendMessage выполняет GET /sendMessage с минимальным набором полей.
// Возвращает (permanent, err):
//
//   - permanent=true, err!=nil  — ошибка 4xx, адресат фиксируется как постоянная неудача;
//   - permanent=false, err!=nil — временная ошибка или сетевой сбой (в том числе retry_after);
//   - permanent=false, err==nil — успех.
//
// При наличии троттлера запрос выполняется внутри limiter.Do().
func (s *BotSender) sendMessage(ctx context.Context, chatID int64, text string) (bool, error) {
	if err := s.limiter.Wait(ctx); err != nil {
		return false, err
	}
	return s.performSend(ctx, chatID, text)
}

// performSend выполняет запрос без троттлера. Обрабатывает HTTP/JSON ответы и
// приводит их к паре (permanent, error).
func (s *BotSender) performSend(ctx context.Context, chatID int64, text string) (bool, error) {
	params := url.Values{}
	params.Set("chat_id", strconv.FormatInt(chatID, 10))
	params.Set("text", text)
	params.Set("disable_web_page_preview", "true")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"?"+params.Encode(), nil)
	if err != nil {
		return false, err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}

	if resp.StatusCode != http.StatusOK {
		return handleHTTPError(resp, body)
	}

	return handleJSONResponse(body)
}

// sendMessageRich отправляет текст с entities (Bot API), сохраняя тот же троттлинг.
// Используется после успешной доставки обычного текста, если включён режим «копии».
func (s *BotSender) sendMessageRich(
	ctx context.Context, chatID int64, text string,
	entities []notifications.CopyEntity,
) (bool, error) {
	if err := s.limiter.Wait(ctx); err != nil {
		return false, err
	}
	return s.performSendRich(ctx, chatID, text, entities)
}

// performSendRich делает POST JSON на /sendMessage с полем entities.
//
// Замечания:
//   - Content-Type: application/json;
//   - DisableWebPagePreview=true, чтобы не было лишних превью;
//   - BODY формируется через json.Marshal. TODO: можно заменить strings.NewReader(string(body)) на bytes.NewReader(body).
func (s *BotSender) performSendRich(
	ctx context.Context, chatID int64, text string,
	entities []notifications.CopyEntity,
) (bool, error) {
	payload := struct {
		ChatID                int64                      `json:"chat_id"`
		Text                  string                     `json:"text"`
		Entities              []notifications.CopyEntity `json:"entities,omitempty"`
		DisableWebPagePreview bool                       `json:"disable_web_page_preview,omitempty"`
	}{
		ChatID:                chatID,
		Text:                  text,
		Entities:              entities,
		DisableWebPagePreview: true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL, strings.NewReader(string(body)))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return handleHTTPError(resp, respBody)
	}
	return handleJSONResponse(respBody)
}

// handleHTTPError нормализует не-200 ответы HTTP в (permanent, error).
// 429: пытается извлечь Retry-After из заголовка или JSON-тела и возвращает retryAfterError;
// 4xx: постоянная ошибка; 5xx: временная ошибка.
func handleHTTPError(resp *http.Response, body []byte) (bool, error) {
	status := resp.StatusCode
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(status)
	}

	switch {
	case status == http.StatusTooManyRequests:
		return false, fmt.Errorf("bot api rate limit (%d): %s", status, msg)
	case status >= 400 && status < 500:
		return true, fmt.Errorf("bot api client error (%d): %s", status, msg)
	default:
		return false, fmt.Errorf("bot api server error (%d): %s", status, msg)
	}
}

// handleJSONResponse разбирает JSON Bot API. Возвращает (permanent, error)
// по тем же правилам, учитывая parameters.retry_after.
func handleJSONResponse(body []byte) (bool, error) {
	var apiResp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		ErrorCode   int    `json:"error_code"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return false, fmt.Errorf("bot api decode response: %w", err)
	}

	if apiResp.OK {
		return false, nil
	}

	msg := strings.TrimSpace(apiResp.Description)
	if msg == "" {
		msg = "(empty bot api description)"
	}

	if apiResp.ErrorCode == http.StatusTooManyRequests {
		return false, fmt.Errorf("bot api rate limit (%d): %s", apiResp.ErrorCode, msg)
	}

	if isPermanentBotError(apiResp.ErrorCode, apiResp.Description) {
		return true, fmt.Errorf("bot api error %d: %s", apiResp.ErrorCode, msg)
	}

	return false, fmt.Errorf("bot api error %d: %s", apiResp.ErrorCode, msg)
}

// parseRetryAfterHeader парсит Retry-After из заголовка: либо число секунд, либо абсолютную дату.
// Возвращает 0, если значение отсутствует или некорректно.
func parseRetryAfterHeader(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}

	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}

	if ts, err := http.ParseTime(value); err == nil {
		delta := time.Until(ts)
		if delta > 0 {
			return delta
		}
	}

	return 0
}

// parseRetryAfterBody извлекает parameters.retry_after из JSON тела. Нулевое или отрицательное — как отсутствие.
func parseRetryAfterBody(body []byte) time.Duration {
	var payload struct {
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0
	}
	if payload.Parameters.RetryAfter <= 0 {
		return 0
	}
	return time.Duration(payload.Parameters.RetryAfter) * time.Second
}

// isPermanentBotError анализирует JSON-ответ Bot API: большинство 4xx — постоянные ошибки,
// но retry_after сигнализирует о временном сбое. Проверка остаётся на случай нестандартных сообщений.
func isPermanentBotError(code int, desc string) bool {
	if code == http.StatusTooManyRequests {
		return false
	}
	desc = strings.ToLower(desc)
	if strings.Contains(desc, "retry_after") || strings.Contains(desc, "retry after") {
		return false
	}
	return code >= 400 && code < 500
}


