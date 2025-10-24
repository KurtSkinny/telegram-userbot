// Package telegramnotifier — вспомогательные инструменты для уведомителя Telegram.
// В данном файле реализованы экстракторы ожиданий (wait extractors) для MTProto-
// транспорта. Они преобразуют специфичные ошибки Telegram API (например FLOOD_WAIT)
// в длительность паузы, понятную троттлеру. Экстракторы не зависят от конкретной
// реализации throttle.WaitExtractor и могут использоваться повторно в других адаптерах.
package telegramnotifier

import (
	rand "math/rand/v2"
	"time"

	"telegram-userbot/internal/infra/throttle"

	"github.com/gotd/td/tgerr"
)

// floodWaitJitterMax — верхняя граница случайного джиттера, добавляемого к обязательному
// FLOOD_WAIT. Добавка нужна, чтобы разнести повторные запросы разных воркеров и снизить
// риск одновременного повторного входа в лимит Telegram.
const floodWaitJitterMax = 3 * time.Second

// FloodWaitExtractor создаёт throttle.WaitExtractor, который распознаёт ошибки
// FLOOD_WAIT и FLOOD_PREMIUM_WAIT из Telegram API. Возвращает пару (delay, true),
// где delay = обязательная пауза из ошибки + случайный джиттер до floodWaitJitterMax.
// Если ошибка не связана с лимитами (tgerr.AsFloodWait(err) == false), возвращает (0, false).
// Используется троттлером для корректного ожидания между повторными запросами.
func FloodWaitExtractor() throttle.WaitExtractor { // экспортируем для явного подключения в провайдере
	return func(err error) (time.Duration, bool) {
		if err == nil {
			return 0, false
		}

		// Разворачиваем цепочку ошибок, чтобы извлечь FLOOD_WAIT.
		// Если err не имеет типа tgerr.FloodWait, ok будет false.
		wait, ok := tgerr.AsFloodWait(err)
		if !ok {
			return 0, false
		}

		// Добавляем небольшой случайный джиттер, чтобы избежать синхронных повторов.
		j := nextFloodWaitJitter()
		return wait + j, true
	}
}

// nextFloodWaitJitter возвращает случайную временную добавку из диапазона [0, floodWaitJitterMax).
// Используется только для FLOOD_WAIT, чтобы сгладить пики повторов при массовых запросах.
// Используем math/rand/v2 (Go ≥ 1.25): функции потокобезопасны, отдельный RNG не требуется.
func nextFloodWaitJitter() time.Duration {
	if floodWaitJitterMax <= 0 {
		return 0
	}
	sec := int(floodWaitJitterMax / time.Second)
	if sec <= 0 {
		return 0
	}
	// math/rand/v2 потокобезопасен, #nosec оправдан — криптографическая стойкость здесь не требуется.
	return time.Duration(rand.IntN(sec)) * time.Second // #nosec G404
}
