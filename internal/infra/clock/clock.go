// Package clock DEPRECATED: используйте пакет apptime вместо этого
// Этот пакет оставлен для обратной совместимости
package clock

import (
	"telegram-userbot/internal/infra/apptime"
	"time"
)

// Now DEPRECATED: используйте apptime.Now() вместо этого
func Now() time.Time {
	return apptime.Now()
}
