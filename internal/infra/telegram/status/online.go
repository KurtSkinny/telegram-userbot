// File online.go: сигналы активности и переходы online/offline.
// Содержит публичные триггеры GoOnline/GoOnlineMinMs, внутренний ping,
// вычисление случайной задержки до авто‑offline и основной цикл run().
package status

import (
	"context"
	"math/rand/v2"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/telegram/connection"

	// "telegram-userbot/internal/infra/telegram/connection"
	"time"
)

const offlineGraceTimeout = 2 * time.Second

// ping сообщает менеджеру о свежей активности. Сбрасывает таймер простоя и удерживает online.
// Канал буферизован на 1 элемент, поэтому всплески пингов схлопываются до одного сигнала.
// Конкурентно безопасно; при заполненном буфере новый сигнал игнорируется без потери актуальности.
func (m *StatusManager) ping(wait int) {
	select {
	case m.pingCh <- wait:
	default:
		// если буфер уже полон — значит, таймер уже сброшен, можно игнорировать
	}
}

// GoOnline немедленно инициирует переход аккаунта в online через глобальный менеджер.
// Если менеджер не инициализирован — функция молча возвращает.
// Время до авто‑ухода в offline выбирается случайно из двух диапазонов (короткий/длинный)
// с вероятностями 80/20, чтобы поведение выглядело менее шаблонным.
func GoOnline() {
	if manager == nil {
		return
	}

	const (
		shortMin   = 5678  // минимальное время для короткого диапазона (мс)
		shortMax   = 12345 // максимальное время для короткого диапазона (мс)
		longMin    = 34567 // минимальное время для длинного диапазона (мс)
		longMax    = 45678 // максимальное время для длинного диапазона (мс)
		shortRatio = 0.8   // вероятность выбора короткого диапазона (80%)
	)
	var minMs, maxMs int

	// Нужна псевдослучайность, криптостойкость не требуется (поэтому math/rand).
	ratio := rand.Float64() // #nosec G404
	if ratio < shortRatio {
		minMs, maxMs = shortMin, shortMax
	} else {
		minMs, maxMs = longMin, longMax
	}

	manager.ping(randomMs(minMs, maxMs))
}

// GoOnlineMinMs — вариант GoOnline с явными границами окна ожидания до offline.
// Если max < min, пишет ошибку в лог и откатывается к GoOnline() с дефолтными диапазонами.
func GoOnlineMinMs(minMs, maxMs int) {
	if manager == nil {
		return
	}

	if maxMs < minMs {
		logger.Error("GoOnlineMinMs: max < min; used GoOnline() instead")
		GoOnline()
		return
	}
	manager.ping(randomMs(minMs, maxMs))
}

// randomMs выбирает равномерное целое в миллисекундах из диапазона [minMs, maxMs].
// Пограничные значения включены. Защищается от перепутанных границ.
func randomMs(minMs, maxMs int) int {
	// защитимся от перепутанных границ
	if maxMs < minMs {
		minMs, maxMs = maxMs, minMs
	}
	// равномерно в диапазоне [minMs, maxMs]
	return rand.IntN(maxMs-minMs+1) + minMs // #nosec G404
}

// setOnlineOnce переводит статус в online, если последний апдейт был более минуты назад.
// Это снижает шум AccountUpdateStatus при частых пингах. Пытается дождаться соединения и
// вызывает AccountUpdateStatus(ctx, false). Обновляет online и lastOnlineAt по успешному вызову.
func (m *StatusManager) setOnlineOnce(ctx context.Context, online *bool, lastOnlineAt *time.Time) {
	if online == nil || lastOnlineAt == nil || m == nil {
		return
	}
	if *online && time.Since(*lastOnlineAt) < time.Minute {
		return
	}
	connection.WaitOnline(ctx)
	if _, err := m.api.AccountUpdateStatus(ctx, false); err != nil {
		connection.HandleError(err)
		logger.Errorf("StatusManager: failed to go online: %v", err)
		return
	}
	logger.Debug("StatusManager: AccountUpdateStatus to online")
	*online = true
	*lastOnlineAt = time.Now()
}

// setOffline переводит аккаунт в offline. Если исходный ctx уже отменён или причина — "context cancel",
// создаёт краткий фоновый контекст с таймаутом offlineGraceTimeout, чтобы успеть отправить запрос на shutdown.
func (m *StatusManager) setOffline(ctx context.Context, reason string, online *bool) {
	if online == nil || m == nil {
		return
	}
	if !*online {
		return
	}

	callCtx := ctx
	var cancel context.CancelFunc
	if ctx.Err() != nil || reason == "context cancel" {
		callCtx, cancel = context.WithTimeout(context.Background(), offlineGraceTimeout)
		defer cancel()
	}

	connection.WaitOnline(callCtx)
	if _, err := m.api.AccountUpdateStatus(callCtx, true); err != nil {
		connection.HandleError(err)
		logger.Errorf("StatusManager: failed to go offline (%s): %v", reason, err)
		return
	}
	logger.Debugf("StatusManager: AccountUpdateStatus to offline (%s)", reason)
	*online = false
}

// run управляет жизненным циклом статуса: реагирует на pingCh, включает online и по таймеру уходит в offline.
// На завершение контекста пытается аккуратно отправить offline и закрывает doneCh. Перед Reset таймера всегда
// выполняется drain его канала, чтобы избежать спурионных тиков.
func (m *StatusManager) run(ctx context.Context) {
	online := false
	lastOnlineAt := time.Now()
	timer := time.NewTimer(time.Hour)
	// Таймер используется только как будильник на авто‑offline; изначально он выключен.
	timer.Stop() // изначально таймер не активен

	for {
		select {
		case <-ctx.Done():
			// контекст завершён — выключаем онлайн и завершаем горутину
			m.setOffline(ctx, "context cancel", &online)
			close(m.doneCh)
			return
		case waitMs := <-m.pingCh:
			// получен "пинг" — активность обнаружена, продлеваем онлайн-сессию
			m.setOnlineOnce(ctx, &online, &lastOnlineAt)
			// Перед Reset нужно остановить и осушить канал таймера, иначе можно поймать старый тик.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			randomTimeout := time.Duration(waitMs) * time.Millisecond
			logger.Debugf("StatusManager: activity detected, next offline in %v", randomTimeout)
			timer.Reset(randomTimeout)

		case <-timer.C:
			// истёк таймаут без активности — переходим в офлайн и ждём новых событий
			m.setOffline(ctx, "idle timeout", &online)
		}
	}
}
