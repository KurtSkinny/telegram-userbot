// Package connection — менеджер состояния MTProto‑соединения.
// Он предоставляет глобальный координационный слой для остального кода:
//   - WaitOnline(ctx) — блокирует до восстановления связи, если клиент офлайн;
//   - MarkConnected/MarkDisconnected — явные переходы между состояниями;
//   - мониторинг с периодическими RPC-вызовами и детекцией сетевых сбоев;
//   - безопасная остановка и «генерационный» канал ожидания для снятия гонок.
//
// Менеджер потокобезопасен: взаимодействие с ожидателями ведётся через снимки
// wait‑канала, а сетевые ошибки нормализуются через HandleError.
package connection

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/storage"
	"telegram-userbot/internal/support/debug"

	"github.com/gotd/td/pool"
	"github.com/gotd/td/rpc"
	"github.com/gotd/td/telegram"
)

const (
	// reconnectPingInterval определяет период, с которым выполняются легковесные RPC-вызовы
	// при ожидании восстановления соединения.
	reconnectPingInterval = 10 * time.Second
	// reconnectPingTimeout задает максимальное время ожидания ответа на RPC-вызов.
	reconnectPingTimeout = 5 * time.Second
	// // pingAbortedAttempts ограничивает число попыток RPC-вызовов, когда соединение явно закрыто.
	// // После этого монитор перестаёт выполнять RPC-вызовы до следующего MarkDisconnected.
	// pingAbortedAttempts = 10
)

var (
	// globalMu защищает глобальные переменные менеджера от гонок.
	globalMu sync.RWMutex
	// globalManager содержит единственный экземпляр менеджера, разделяемый приложением.
	globalManager *manager
)

// Shutdown завершает глобальный менеджер соединений, отменяет фоновый мониторинг
// и закрывает каналы ожидания, чтобы разблокировать все зависшие горутины.
func Shutdown() {
	globalMu.Lock()
	m := globalManager
	globalManager = nil
	globalMu.Unlock()

	if m != nil {
		m.shutdown()
	}
}

// manager хранит ссылку на клиент, текущее состояние online/offline и «поколенческий»
// канал ожидания восстановления (waitCh). Когда связь теряется, создаётся новый
// открытый канал и стартует monitorLoop; при восстановлении канал закрывается, что
// неблокирующим образом снимает все ожидатели. Доступ к полям защищён мьютексами,
// признак online хранится в atomic.Bool.
type manager struct {
	client *telegram.Client // клиент Telegram, используемый для выполнения RPC-вызовов
	ctx    context.Context  // базовый контекст жизненного цикла менеджера

	connected atomic.Bool // признак, что клиент в онлайне

	mu            sync.RWMutex       // защищает waitCh и monitorCancel
	waitCh        chan struct{}      // канал, закрывающийся при восстановлении соединения
	monitorCancel context.CancelFunc // отменяет текущий цикл мониторинга
}

// Init инициализирует глобальный менеджер поверх заданного tg‑клиента и контекста
// жизненного цикла. По умолчанию состояние — online: создаётся закрытый waitCh,
// чтобы текущие вызовы WaitOnline не блокировались. Повторный вызов перетирает
// предыдущий инстанс менеджера.
func Init(ctx context.Context, client *telegram.Client) {
	if client == nil {
		return
	}

	m := &manager{
		client: client,
		ctx:    ctx,
	}

	// Стартуем в состоянии online: ожидатели не должны блокироваться «на ровном месте».
	m.connected.Store(true)
	// Создаём и сразу закрываем канал ожидания: снимок для WaitOnline в «онлайне».
	ready := make(chan struct{})
	close(ready)
	m.waitCh = ready

	globalMu.Lock()
	globalManager = m
	globalMu.Unlock()
}

// MarkConnected переводит глобальное состояние в online, останавливает мониторинг
// и закрывает текущий wait‑канал, разблокируя всех ожидателей.
func MarkConnected() {
	if m := getManager(); m != nil {
		m.markConnected()
	}
}

// MarkDisconnected переводит глобальное состояние в offline. Идемпотентен: если
// уже офлайн — ничего не делает. Создаёт новое «поколение» wait‑канала и запускает
// мониторинг восстановления (monitorLoop).
func MarkDisconnected() {
	if m := getManager(); m != nil {
		m.markDisconnected()
	}
}

// WaitOnline блокирует вызывающую горутину до восстановления соединения или отмены
// контекста. Если уже online, возвращает сразу. Логика использует «снимки» канала
// ожидания: если мы проснулись по старому закрытому каналу, цикл продолжится до
// закрытия актуального канала текущего поколения.
func WaitOnline(ctx context.Context) {
	if ctx == nil || ctx.Err() != nil {
		return
	}

	m := getManager()
	if m == nil {
		return
	}

	if m.connected.Load() {
		return
	}

	// Для удобства диагностики логируем место вызова, которое заблокировалось в ожидании.
	callerLocation := "unknown"
	if _, file, line, ok := runtime.Caller(1); ok {
		if wd, err := os.Getwd(); err == nil {
			if rel, relErr := filepath.Rel(wd, file); relErr == nil {
				file = rel
			}
		}
		callerLocation = file + ":" + strconv.Itoa(line)
	}

	logger.Debugf("WaitOnline: blocking caller: %s", callerLocation)

	for {
		// Берём моментальный снимок канала текущего поколения.
		ch := m.currentWaitCh() // не-nil снимок
		select {
		case <-ctx.Done():
			logger.Debugf("WaitOnline: context done before reconnect: %v", ctx.Err())

			return
		case <-ch:
			// Проснулись. Если это канал актуального поколения — можно продолжать работу.
			if ch == m.currentWaitCh() { // закрыли актуальный канал
				logger.Debug("WaitOnline: connection restored, resuming")
				return
			}
			// попали на старый закрытый — ждём дальше
		}
	}
}

// // DoOnline ждёт восстановление соединения (если нужно) и затем вызывает fn.
// // Если fn вернул ошибку — она прогоняется через HandleError и возвращается вызывающему.
// func DoOnline(ctx context.Context, fn func(context.Context) error) error {
// 	WaitOnline(ctx)
// 	if err := fn(ctx); err != nil {
// 		HandleError(err)
// 		return err
// 	}
// 	return nil
// }

// // CallOnline аналогичен DoOnline, но возвращает произвольный результат.
// // Тип результата выводится из сигнатуры переданной функции.
// func CallOnline[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
// 	WaitOnline(ctx)
// 	v, err := fn(ctx)
// 	if err != nil {
// 		HandleError(err)
// 		return v, err
// 	}
// 	return v, nil
// }

// HandleError анализирует ошибку err, полученную из RPC-слоя. Если ошибка
// напоминает сетевую и свидетельствует о разрыве соединения, менеджер
// переводится в offline, а функция возвращает true. Иначе возвращается false.
func HandleError(err error) bool {
	if !isNetworkError(err) {
		return false
	}

	MarkDisconnected()
	return true
}

// getManager возвращает текущий глобальный менеджер или nil, если тот не
// инициализирован.
func getManager() *manager {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return globalManager
}

// currentWaitCh возвращает снимок актуального канала ожидания. Если канал ещё
// не инициализирован (nil), возвращается закрытый канал, чтобы WaitOnline не
// блокировался по ошибке.
func (m *manager) currentWaitCh() <-chan struct{} {
	m.mu.RLock()
	ch := m.waitCh
	m.mu.RUnlock()
	if ch == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return ch
}

// markConnected помечает менеджер как online: отменяет мониторинг, закрывает
// канал ожидания (если ещё не закрыт) и логирует событие. Идемпотентен.
func (m *manager) markConnected() {
	if m == nil {
		return
	}

	if m.connected.Swap(true) {
		return
	}

	m.mu.Lock()
	if m.monitorCancel != nil {
		m.monitorCancel()
		m.monitorCancel = nil
	}
	ch := m.waitCh
	if ch == nil {
		ch = make(chan struct{})
		m.waitCh = ch
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
	m.mu.Unlock()

	logger.Info("ConnectionMonitor: connection restored")
}

// markDisconnected атомарно переключает состояние из online в offline, создаёт новый
// открытый канал ожидания и запускает monitorLoop в отдельной горутине.
func (m *manager) markDisconnected() {
	if m == nil {
		return
	}

	// Идемпотентный переход в offline: действуем только если были online.
	if !m.connected.CompareAndSwap(true, false) {
		return
	}

	m.mu.Lock()
	// Останавливаем предыдущий мониторинг (если был)
	if m.monitorCancel != nil {
		m.monitorCancel()
		m.monitorCancel = nil
	}
	// Новое поколение канала ожидания: открытый канал = оффлайн
	m.waitCh = make(chan struct{})
	monitorCtx, cancel := context.WithCancel(m.ctx)
	m.monitorCancel = cancel
	m.mu.Unlock()

	logger.Debug("ConnectionMonitor: connection lost, waiting for restore")
	go m.monitorLoop(monitorCtx)
}

// shutdown мягко останавливает мониторинг и закрывает канал ожидания, гарантируя,
// что все заблокированные ожидатели проснутся и корректно завершатся.
func (m *manager) shutdown() {
	if m == nil {
		return
	}

	m.mu.Lock()
	if m.monitorCancel != nil {
		m.monitorCancel()
		m.monitorCancel = nil
	}
	wait := m.waitCh
	m.waitCh = nil
	m.mu.Unlock()

	if wait != nil {
		select {
		case <-wait:
		default:
			close(wait)
		}
	}
}

// monitorLoop с периодом reconnectPingInterval пытается выполнить RPC-вызов.
// При успехе менеджер переводится в online и цикл завершается. Закрытые/мертвые соединения
// считаются «abort» попытками и ограничены pingAbortedAttempts. Нечёткие сетевые
// ошибки логируются, контекстная отмена завершает цикл без шума.
func (m *manager) monitorLoop(ctx context.Context) {
	// Периодические RPC-вызовы позволяют выйти из офлайна без внешнего участия.
	ticker := time.NewTicker(reconnectPingInterval)
	defer ticker.Stop()

	attempt := 0

	for {
		if ctx.Err() != nil {
			return
		}

		// Считаем попытки: важно для порога pingAbortedAttempts при закрытом коннекте.
		attempt++
		start := time.Now()

		client := m.client
		if client == nil {
			// Клиент ещё не присвоен — ждём следующего тика, не паникуем.
			logger.Debugf("ConnectionMonitor: client is nil, waiting for reconnect (attempt=%d)", attempt)
		} else {
			// Выполняем легковесный RPC-вызов с ограничением по времени.
			// Используем Self(), так как это легковесный вызов, который
			// требует полноценного MTProto-соединения и готов к работе API.
			pingCtx, cancel := context.WithTimeout(ctx, reconnectPingTimeout)
			err := m.safeRPCClient(pingCtx, client)
			cancel()

			if err == nil {
				logger.Debugf("ConnectionMonitor: RPC call ok (attempt=%d, duration=%v)", attempt, time.Since(start))
				m.markConnected()
				return
			}

			switch {
			// Явно закрытое соединение или умерший движок: считаем «abort» попыткой.
			case errors.Is(err, net.ErrClosed), errors.Is(err, pool.ErrConnDead), errors.Is(err, rpc.ErrEngineClosed):
				logger.Debugf("ConnectionMonitor: RPC call aborted, connection closed (attempt=%d, duration=%v): %v", attempt, time.Since(start), err)
				// if attempt >= pingAbortedAttempts {
				// 	logger.Debugf("ConnectionMonitor: RPC monitor loop aborted, max attempts (%d) reached", pingAbortedAttempts)
				// 	return
				// }
			case !isNetworkError(err):
				logger.Errorf("ConnectionMonitor: RPC call failed (attempt=%d, duration=%v): %v", attempt, time.Since(start), err)
			default:
				logger.Debugf("ConnectionMonitor: RPC call failed (attempt=%d, duration=%v): %v", attempt, time.Since(start), err)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// safeRPCClient оборачивает легковесный RPC-вызов (Self) защитой от паник
// и переводит их в сетевую ошибку (net.ErrClosed). При nil‑клиенте сразу возвращает net.ErrClosed.
// Этот метод проверяет, что MTProto-клиент полностью готов к работе, а не просто пингуется.
func (m *manager) safeRPCClient(ctx context.Context, client *telegram.Client) (err error) {
	if client == nil {
		return net.ErrClosed
	}
	defer func() {
		if r := recover(); r != nil {
			logger.Debugf("ConnectionMonitor: RPC call panic recovered: %v", r)
			err = net.ErrClosed
		}
	}()

	// Выполняем легковесный RPC-вызов, который требует полноценного MTProto-соединения
	// и готовности API к работе. Self() является легковесным вызовом,
	// который проверяет работоспособность соединения и готовность API.
	// Используем Self() вместо пинга, потому что он дает лучшую гарантию готовности MTProto-клиента
	// к выполнению других API-запросов, в отличие от пинга, который может проходить до полной
	// инициализации MTProto-соединения.
	_, err = client.Self(ctx)
	return err
}

// isNetworkError определяет, сигнализирует ли ошибка о сетевой проблеме/разрыве.
// Считаем сетевыми: закрытия соединения/движка (pool.ErrConnDead, rpc.ErrEngineClosed),
// исчерпание ретраев rpc.RetryLimitReachedErr, таймауты/дедлайны, EOF и net.Error.
// Контекстные отмены не считаем сетевыми. Для отладки логируем прочие ошибки в файл.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.Canceled) {
		return false
	}

	if errors.Is(err, pool.ErrConnDead) {
		return true
	}
	if errors.Is(err, rpc.ErrEngineClosed) {
		return true
	}
	var retryErr *rpc.RetryLimitReachedErr
	if errors.As(err, &retryErr) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) { //nolint: S1008 // because
		return true
	}

	if debug.DEBUG {
		logger.Warnf("isNetworkError: %v", err)
		logNonNetworkError(err)
	}

	return false
}

// networkErrorsLogPath — путь файла, куда пишутся «не сетевые» ошибки для диагностики.
const networkErrorsLogPath = "/data/isnetworkerrors.log"

// logNonNetworkError добавляет запись в диагностический лог, используя атомарную
// запись файла. Ошибки чтения/записи логируются на debug‑уровне и не фатальны.
func logNonNetworkError(err error) {
	entry := time.Now().UTC().Format(time.RFC3339Nano) + "\t" + err.Error() + "\n"

	data, readErr := os.ReadFile(networkErrorsLogPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		logger.Debugf("isNetworkError: cannot read %s: %v", networkErrorsLogPath, readErr)
		return
	}

	if writeErr := storage.AtomicWriteFile(networkErrorsLogPath, append(data, entry...)); writeErr != nil {
		logger.Debugf("isNetworkError: cannot write %s: %v", networkErrorsLogPath, writeErr)
	}
}
