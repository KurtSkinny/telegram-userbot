package throttle

// Package throttle — общий механизм ограничения скорости и повторных попыток для внешних интеграций.
// В основе — токен-бакет (RPS + burst) и экспоненциальный backoff с джиттером.
// Поддерживаются серверные указания подождать (retry_after, FLOOD_WAIT и т.п.) через настраиваемые
// WaitExtractor. Интерфейс StopRetryer позволяет немедленно прекращать ретраи.
// Троттлер потокобезопасен: Do может вызываться параллельно; Start/Stop идемпотентны.

import (
	"context"
	"errors"
	"fmt"
	"math"

	// "math/rand"
	"math/rand/v2"
	"sync"
	"time"
)

// burstBultiplier задаёт burst по умолчанию как кратный rate. Значение 2 означает
// способность кратковременно «впрыснуть» до 2*rate операций в секунду.
const burstBultiplier = 2

// WaitExtractor анализирует ошибку и, при необходимости, возвращает длительность ожидания.
// Возвращаемый булев флаг показывает, что экстрактор распознал формат ошибки.
// Экстракторы вызываются последовательно в порядке регистрации, первый совпавший
// определяет паузу перед повторной попыткой.
type WaitExtractor func(err error) (time.Duration, bool)

// StopRetryer объявляет необходимость немедленно прекратить повторные попытки.
// Любая ошибка, реализующая этот интерфейс, возвращается вызывающему коду без задержек.
type StopRetryer interface {
	StopRetry() bool
}

// Option задаёт дополнительные параметры троттлера при создании.
type Option func(*Throttler)

// WithMaxRetries ограничивает количество повторных попыток. Значение <=0 означает отсутствие ограничения.
func WithMaxRetries(maxRetries int) Option {
	return func(t *Throttler) {
		t.maxRetries = maxRetries
	}
}

// WithBurst переопределяет ёмкость токен-бакета (число накопленных токенов).
// Если burst <= 0, будет использовано значение по умолчанию 2*rate.
func WithBurst(burst int) Option {
	return func(t *Throttler) {
		t.burst = burst
	}
}

// WithWaitExtractors регистрирует набор экстракторов, определяющих серверные задержки.
func WithWaitExtractors(extractors ...WaitExtractor) Option {
	return func(t *Throttler) {
		if len(extractors) == 0 {
			return
		}
		cloned := make([]WaitExtractor, len(extractors))
		copy(cloned, extractors)
		t.waitExtractors = append(t.waitExtractors, cloned...)
	}
}

// WithRand позволяет задать источник случайности. Используется в основном для детерминированных тестов.
func WithRand(r *rand.Rand) Option {
	return func(t *Throttler) {
		if r != nil {
			t.randomFn = r.Float64
		}
	}
}

// WithRandom позволяет задать функцию генерации случайных чисел (для тестов).
func WithRandom(fn func() float64) Option {
	return func(t *Throttler) {
		if fn != nil {
			t.randomFn = fn
		}
	}
}

// ErrNotStarted возвращается, если вызов Do произошёл до запуска Start.
var ErrNotStarted = errors.New("throttle: Start must be called before Do")

// Throttler инкапсулирует токен-бакет (RPS + burst) и стратегию повторных попыток
// с экспоненциальным бэкофом и поддержкой серверных задержек через WaitExtractor.
// Потокобезопасен: Do может выполняться из нескольких горутин, Start/Stop идемпотентны.
type Throttler struct {
	rate  int // сколько токенов пополняется в секунду (базовый RPS)
	burst int // максимальное число накопленных токенов (ёмкость бакета)

	tokens chan struct{} // буферизированный канал-«бакет»; каждый токен разрешает один вызов

	waitExtractors []WaitExtractor // цепочка экстракторов, извлекающих «сколько подождать» из ошибок
	maxRetries     int             // лимит ретраев; -1 означает «без ограничений»

	startOnce sync.Once
	stopOnce  sync.Once
	wg        sync.WaitGroup // ожидание фоновой горутины пополнения

	rootCtx context.Context // корневой контекст жизни троттлера
	cancel  context.CancelFunc

	mu       sync.Mutex     // защищает изменяемые поля и снапшоты
	randomFn func() float64 // источник случайности для джиттера (подменяется в тестах)
}

// New создаёт троттлер с частотой rate (операций/сек). По умолчанию burst = 2*rate
// с нижней границей 1. Опции позволяют задать burst, лимит ретраев, WaitExtractor и
// источник случайности. Start вызывается отдельно для запуска пополнения бакета.
func New(rate int, opts ...Option) *Throttler {
	if rate <= 0 {
		rate = 1
	}

	t := &Throttler{
		rate:       rate,
		burst:      rate * burstBultiplier,
		maxRetries: -1,
	}

	for _, opt := range opts {
		opt(t)
	}

	if t.burst <= 0 {
		t.burst = rate * burstBultiplier
	}
	if t.burst < 1 {
		t.burst = 1
	}

	if t.randomFn == nil {
		t.randomFn = rand.Float64
	}

	return t
}

// Start инициализирует канал токенов, предзаполняет бакет и запускает пополнение.
// Метод идемпотентен; при ctx=nil используется context.Background().
func (t *Throttler) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	t.startOnce.Do(func() {
		t.rootCtx, t.cancel = context.WithCancel(ctx)
		// Создаём «бакет» нужной ёмкости.
		t.tokens = make(chan struct{}, t.burst)
		// Предзаполняем бакет до burst, чтобы не ждать «раскрутки» с нуля.
		for range t.burst {
			t.tokens <- struct{}{}
		}
		// Фоновая горутина пополняет бакет фиксированным интервалом.
		t.wg.Go(func() {
			t.refillLoop()
		})
	})
}

// Stop останавливает пополнение и завершает все фоновые горутины.
// Метод идемпотентен: повторные вызовы безопасны.
func (t *Throttler) Stop() {
	if !t.isStarted() {
		return
	}
	t.stopOnce.Do(func() {
		if t.cancel != nil {
			t.cancel()
		}
		t.wg.Wait()
	})
}

// SetMaxRetries позволяет изменить лимит повторных попыток уже после создания троттлера.
// Значение <=0 продолжает означать «без ограничений». Метод потокобезопасен.
func (t *Throttler) SetMaxRetries(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.maxRetries = n
}

// Do выполняет функцию fn с лимитами токен-бакета и ретраями.
// Алгоритм:
//  1. ждём токен (с уважением к ctx и Stop);
//  2. вызываем fn;
//  3. если err: StopRetryer → вернуть сразу; контекст сорван → вернуть;
//     extractor дал паузу → подождать и повторить без роста attempt;
//     иначе экспоненциальный backoff с джиттером, учитывая лимит ретраев.
//
// Возвращает nil при успехе либо последнюю ошибку при исчерпании стратегии.
func (t *Throttler) Do(ctx context.Context, fn func() error) error {
	// Гвард по контексту.
	if ctx == nil {
		ctx = context.Background()
	}
	// Снимок корневого контекста троттлера.
	root := t.rootContext()
	if root == nil {
		return ErrNotStarted
	}
	// Снимок лимита ретраев: не меняем его внутри, чтобы не наращивать ветвления.
	maxRetries := t.currentMaxRetries()

	attempt := 0
	for {
		// Получаем токен (может прерваться по ctx/root).
		if err := t.takeToken(ctx, root); err != nil {
			return err
		}

		// Вызываем целевую функцию.
		callErr := fn()
		if callErr == nil {
			return nil
		}

		// Классифицируем ошибку одной плоской конструкцией.
		var stopper StopRetryer
		waitDur, hasWait := t.extractWait(callErr)

		switch {
		case errors.As(callErr, &stopper) && stopper.StopRetry():
			// Немедленно отдаём ошибку без ретраев.
			return callErr

		case errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded):
			// Прерываемся, если контекст сорвался.
			return callErr

		case hasWait:
			// Сервер велел подождать — ждём и повторяем без роста attempt.
			if wErr := t.wait(ctx, root, waitDur); wErr != nil {
				return wErr
			}
			continue
		}

		// Лимит ретраев (если задан).
		if maxRetries > 0 && attempt >= maxRetries {
			return fmt.Errorf("throttle: max retries reached (%d): last error: %w", maxRetries, callErr)
		}

		// Экспоненциальный бэкоф + джиттер.
		sleep := t.expBackoff(attempt)
		attempt++
		if wErr := t.wait(ctx, root, sleep); wErr != nil {
			return wErr
		}
	}
}

// rootContext возвращает текущий корневой контекст троттлера под мьютексом.
func (t *Throttler) rootContext() context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rootCtx
}

// isStarted сообщает, инициализирован ли троттлер (Start был вызван).
func (t *Throttler) isStarted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rootCtx != nil
}

// currentMaxRetries возвращает снапшот лимита ретраев под мьютексом.
func (t *Throttler) currentMaxRetries() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.maxRetries
}

// takeToken блокирует до получения токена или отмены контекста. При остановке
// троттлера возвращает context.Canceled, что согласовано с общим флоу Do.
func (t *Throttler) takeToken(ctx, rootCtx context.Context) error {
	tokenCh := t.tokenChannel()
	if tokenCh == nil {
		return ErrNotStarted
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-rootCtx.Done():
		return context.Canceled
	case <-tokenCh:
		return nil
	}
}

// tokenChannel возвращает снимок канала токенов под мьютексом.
func (t *Throttler) tokenChannel() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tokens
}

// refillLoop с периодом 1/rate добавляет токен в бакет, не переполняя burst.
func (t *Throttler) refillLoop() {
	rootCtx := t.rootContext()
	if rootCtx == nil {
		return
	}

	interval := time.Second / time.Duration(t.rate)
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-rootCtx.Done():
			return
		case <-ticker.C:
			select {
			case t.tokens <- struct{}{}:
			default:
			}
		}
	}
}

// extractWait запускает WaitExtractor по цепочке и возвращает первую распознанную паузу.
func (t *Throttler) extractWait(err error) (time.Duration, bool) {
	for _, extractor := range t.waitExtractors {
		if extractor == nil {
			continue
		}
		if wait, ok := extractor(err); ok {
			return wait, true
		}
	}
	return 0, false
}

// wait ждёт duration или отмену любого из контекстов (внешнего/корневого).
func (t *Throttler) wait(ctx, rootCtx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}

	timer := time.NewTimer(duration)
	defer stopTimer(timer)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-rootCtx.Done():
		return context.Canceled
	case <-timer.C:
		return nil
	}
}

// expBackoff вычисляет задержку 2^attempt секунд, ограниченную 60с и умноженную на
// джиттер из диапазона [0.85..1.15]. Возвращает значение в time.Duration.
func (t *Throttler) expBackoff(attempt int) time.Duration {
	const (
		jitterRange = 0.3
		jitterMin   = 0.85
		maxSeconds  = 60.0
		basePower   = 2.0
	)

	base := math.Pow(basePower, float64(attempt))
	if base > maxSeconds {
		base = maxSeconds
	}

	jitter := t.random()*jitterRange + jitterMin
	seconds := base * jitter
	return time.Duration(seconds * float64(time.Second))
}

// random возвращает псевдослучайное число в [0,1). Источник можно подменить опциями New.
func (t *Throttler) random() float64 {
	// randomFn задаётся при создании; чтение без блокировок безопасно
	// при условии, что опции не меняются после New.
	if t.randomFn == nil {
		return rand.Float64() // #nosec G404
	}
	return t.randomFn()
}

// stopTimer безопасно останавливает таймер и дренирует его канал, если тик уже произошёл.
func stopTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
