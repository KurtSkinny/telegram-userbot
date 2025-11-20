package clock

import (
	"telegram-userbot/internal/infra/config"
	"time"
)

// Now возвращает текущее время в глобальной таймзоне приложения.
func Now() time.Time {
	return time.Now().In(config.AppLocation)
}
