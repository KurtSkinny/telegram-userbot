package status

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/telegram/connection"
	"time"

	"github.com/gotd/td/telegram"
)

// statusManager управляет онлайн-статусом пользователя в Telegram.
// Реагирует на сигналы активности (пинги) и поддерживает аккаунт в online,
// а при отсутствии активности уводит в offline по таймауту.
// Также предоставляет методы для эмуляции статуса "печатает".
type statusManager struct {
	client *telegram.Client
	pingCh chan int      // Буферизованный канал сигналов активности (пинги)
	doneCh chan struct{} // Канал для сигнализации о завершении работы менеджера
}

// Глобальное хранение синглтона менеджера и его cancel-функции. Доступ защищён mutex-ом.
var (
	manager *statusManager // manager хранит глобальный экземпляр менеджера статуса для всего процесса

	statusWg     sync.WaitGroup
	statusMu     sync.Mutex
	statusCancel context.CancelFunc
)

// Start инициализирует и запускает глобальный менеджер статуса с переданным tg.Client.
// Если менеджер уже запущен, вызов игнорируется.
// clientCtx используется как родительский контекст для жизненного цикла менеджера.
func Start(ctx context.Context, client *telegram.Client) {
	if manager != nil {
		return
	}

	statusMu.Lock()
	defer statusMu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	// Отдельный под-контекст, чтобы можно было целенаправленно гасить менеджер из Shutdown().
	manager = &statusManager{
		client: client,
		pingCh: make(chan int, 1),
		doneCh: make(chan struct{}),
	}
	// pingCh имеет размер 1, поэтому частые пинги будут схлопываться до одного непроцессенного сигнала.
	statusCancel = cancel
	statusWg.Go(func() {
		manager.run(runCtx, ctx)
	})
}

// Stop останавливает менеджер: Cancel контекста → ожидание закрытия doneCh. Повторные вызовы — безопасны.
func Stop() {
	statusMu.Lock()
	m := manager
	cancel := statusCancel
	manager = nil
	statusCancel = nil
	statusMu.Unlock()

	if m == nil {
		return
	}

	if cancel != nil {
		cancel()
	}
	statusWg.Wait()
}

// ping сообщает менеджеру о свежей активности. Сбрасывает таймер простоя и удерживает online.
// Канал буферизован на 1 элемент, поэтому всплески пингов схлопываются до одного сигнала.
// Конкурентно безопасно; при заполненном буфере новый сигнал игнорируется без потери актуальности.
func (m *statusManager) ping(wait int) {
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
	callerLocation := "unknown"
	if _, file, line, ok := runtime.Caller(1); ok {
		if wd, err := os.Getwd(); err == nil {
			if rel, relErr := filepath.Rel(wd, file); relErr == nil {
				file = rel
			}
		}
		callerLocation = file + ":" + strconv.Itoa(line)
	}
	logger.Debugf("GoOnline: caller: %s", callerLocation)

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

// goOnlineInterval — вариант GoOnline с явными границами окна ожидания до offline.
// Если max < min, пишет ошибку в лог и откатывается к GoOnline() с дефолтными диапазонами.
func goOnlineInterval(minMs, maxMs int) {
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

// setOnline переводит статус в online, если последний апдейт был более минуты назад.
// Это снижает шум AccountUpdateStatus при частых пингах. Пытается дождаться соединения и
// вызывает AccountUpdateStatus(ctx, false). Обновляет online и lastOnlineAt по успешному вызову.
func (m *statusManager) setOnline(ctx context.Context, online *bool, lastOnlineAt *time.Time) {
	if online == nil || lastOnlineAt == nil || m == nil {
		return
	}
	if *online && time.Since(*lastOnlineAt) < time.Minute {
		return
	}
	connection.WaitOnline(ctx)
	if _, err := m.client.API().AccountUpdateStatus(ctx, false); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			logger.Debugf("StatusManager: context cancelled while going online (%v)", err)
			return
		}
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
func (m *statusManager) setOffline(ctx context.Context, reason string, online *bool) {
	if online == nil || m == nil {
		return
	}
	if !*online {
		return
	}

	connection.WaitOnline(ctx)
	if _, err := m.client.API().AccountUpdateStatus(ctx, true); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			logger.Debugf("StatusManager: context cancelled while going offline (%v)", err)
			return
		}
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
func (m *statusManager) run(runCtx context.Context, clientCtx context.Context) {
	online := false
	lastOnlineAt := time.Now()
	timer := time.NewTimer(time.Hour)
	// Таймер используется только как будильник на авто‑offline; изначально он выключен.
	timer.Stop() // изначально таймер не активен

	for {
		select {
		case <-runCtx.Done():
			m.setOffline(clientCtx, "exiting", &online)
			close(m.doneCh)
			return
		case waitMs := <-m.pingCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			// получен "пинг" — активность обнаружена, продлеваем онлайн-сессию
			m.setOnline(clientCtx, &online, &lastOnlineAt)
			// Перед Reset нужно остановить и осушить канал таймера, иначе можно поймать старый тик.
			randomTimeout := time.Duration(waitMs) * time.Millisecond
			logger.Debugf("StatusManager: activity detected, next offline in %v", randomTimeout)
			timer.Reset(randomTimeout)

		case <-timer.C:
			// истёк таймаут без активности — переходим в офлайн и ждём новых событий
			m.setOffline(clientCtx, "idle timeout", &online)
		}
	}
}
