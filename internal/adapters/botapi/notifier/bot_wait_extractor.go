package botapionotifier

// Package botapionotifier — вспомогательные инструменты для уведомителя на Bot API.
// В этом файле реализован экстрактор ожиданий для общего троттлера: он извлекает
// серверный параметр retry_after из ошибок и возвращает точную длительность паузы.
// Важно: здесь без джиттера — интервал, который отдаёт сервер, соблюдается ровно,
// чтобы не сдвигать серверную «окну» повторных попыток.

import (
	"errors"
	"time"

	"telegram-userbot/internal/infra/throttle"
)

// retryAfterProvider — облегчённый контракт для ошибок Bot API, которые могут
// нести параметр retry_after. Конкретные типы ошибок приводятся к нему через
// errors.As; реализация скрыта в адаптере Bot API.
type retryAfterProvider interface {
	RetryAfter() time.Duration
}

// BotAPIRetryAfterExtractor создаёт throttle.WaitExtractor, извлекающий retry_after
// из ошибки (через интерфейс retryAfterProvider). Возвращает (delay, true), если значение
// положительное; иначе (0, false), и троттлер применит общую стратегию backoff.
// Джиттер не добавляется принципиально, чтобы строго следовать Bot API.
func BotAPIRetryAfterExtractor() throttle.WaitExtractor {
	return func(err error) (time.Duration, bool) {
		if err == nil {
			// Нет ошибки — ждать нечего, передаём управление другим экстракторам/политике бэкоффа.
			return 0, false
		}

		// Разворачиваем цепочку ошибок и пытаемся получить retry_after через интерфейс.
		var provider retryAfterProvider
		if !errors.As(err, &provider) {
			return 0, false
		}

		// Сервер прислал точное время ожидания. Забираем «как есть», без пересчётов.
		wait := provider.RetryAfter()
		// Нулевое/отрицательное значение считаем отсутствием рекомендации — отдаём (0, false).
		if wait <= 0 {
			return 0, false
		}
		// Возвращаем серверный интервал ожидания без джиттера.
		return wait, true
	}
}
