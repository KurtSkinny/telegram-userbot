// Package notifications реализует очередь уведомлений: постановку задач,
// планирование отправки, взаимодействие с подготовленным отправщиком и
// персистентное хранение состояния/ошибок. Очередь рассчитана на долгую работу,
// переживает рестарты (persist/restore), соблюдает приоритет срочных задач
// и поддерживает расписание для регулярных рассылок.

package notifications

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"telegram-userbot/internal/domain/filters"
	"telegram-userbot/internal/domain/recipients"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/telegram/connection"
	"telegram-userbot/internal/infra/telegram/peersmgr"

	"github.com/gotd/td/tg"
)

// warnIfLargeSize — эвристический порог, при превышении которого в лог пишется предупреждение
// о накоплении задач. Значение 1000 выбрано, потому что очередь со стольким количеством
// элементов уже требует внимания, но при этом не критично для памяти.
const warnIfLargeSize = 1000

// scheduleKeyResolution определяет гранулярность дедупликации триггеров расписания
const scheduleKeyResolution = time.Minute

// PreparedSender — транспорт доставки подготовленных заданий очереди.
// Реализации обязаны обеспечивать идемпотентность (не повторять уже доставленное),
// разумный троттлинг и стратегию ретраев. Опционально могут реализовывать
// интерфейсы жизненного цикла Start/Stop и хук BeforeDrain.
type PreparedSender interface {
	Deliver(ctx context.Context, job Job) (SendOutcome, error)
}

// SendOutcome — результат попытки отправки одного задания.
//   - PermanentFailures — список получателей, которым доставить нельзя (бан, 403 и т.п.);
//   - PermanentError — агрегированное описание причины перманентного сбоя;
//   - NetworkDown — транспорт сообщил об оффлайне; очередь приостановит дренирование и подождёт online;
//   - Retry — рекомендовано повторить попытку позднее (например, 429).
type SendOutcome struct {
	PermanentFailures []Recipient
	PermanentError    error
	NetworkDown       bool
	Retry             bool
}

// QueueOptions — зависимости и параметры очереди: транспорт, сторы, расписание, таймзона и часы.
// Clock допускает внедрение монотонного времени в тестах; по умолчанию используется time.Now.
type QueueOptions struct {
	Sender   PreparedSender
	Store    *QueueStore
	Failed   *FailedStore
	Schedule []string
	Location *time.Location
	Clock    func() time.Time
	Peers    *peersmgr.Service
}

// scheduleEntry — нормализованный слот расписания в локальной таймзоне.
// label хранит исходную строку (например, "09:30") для логирования.
type scheduleEntry struct {
	hour   int
	minute int
	label  string
}

// drainSignal — запрос на дренирование регулярной очереди.
// preHookDone=true означает, что BeforeDrain уже вызван на продьюсер‑пути.
type drainSignal struct {
	reason      string
	preHookDone bool
}

// beforeDrainer объявляет необязательный хук транспорта, вызываемый перед началом дренирования.
type beforeDrainer interface{ BeforeDrain(context.Context) }

// QueueStats — снимок состояния для CLI/мониторинга.
// Важно: NextScheduleAt возвращается в UTC; для отображения используйте Location.
type QueueStats struct {
	Urgent             int
	Regular            int
	LastRegularDrainAt time.Time
	LastFlushAt        time.Time
	NextScheduleAt     time.Time // в UTC
	Location           *time.Location
}

// Queue — основная структура очереди уведомлений.
// Хранит состояние в памяти, синхронизирует его с диском, управляет воркером
// срочных задач и планировщиком регулярных. Потокобезопасность обеспечивается mutex.
type Queue struct {
	sender   PreparedSender
	store    *QueueStore
	failed   *FailedStore
	location *time.Location
	schedule []scheduleEntry
	peers    *peersmgr.Service

	mu    sync.Mutex
	state State

	urgentCh  chan struct{}
	regularCh chan drainSignal

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	now     func() time.Time
	runOnce sync.Once

	// lastScheduledKey хранит ключ последнего триггера расписания (UTC, усечённый до минуты)
	lastScheduledKey int64
}

// NewQueue восстанавливает состояние из хранилища, парсит расписание, подготавливает каналы и зависимости.
// Не запускает воркеры: для старта используйте Start(). Валидирует обязательные опции.
func NewQueue(opts QueueOptions) (*Queue, error) {
	if opts.Sender == nil {
		return nil, errors.New("notifications queue: sender is nil")
	}
	if opts.Store == nil {
		return nil, errors.New("notifications queue: store is nil")
	}
	if opts.Failed == nil {
		return nil, errors.New("notifications queue: failed store is nil")
	}
	if len(opts.Schedule) == 0 {
		return nil, errors.New("notifications queue: schedule is empty")
	}

	location := opts.Location
	if location == nil {
		location = time.UTC
	}

	schedule, err := parseSchedule(opts.Schedule)
	if err != nil {
		return nil, fmt.Errorf("parse schedule: %w", err)
	}

	state, err := opts.Store.Load()
	if err != nil {
		return nil, fmt.Errorf("load queue state: %w", err)
	}

	nowFn := opts.Clock
	if nowFn == nil {
		nowFn = time.Now
	}

	q := &Queue{
		sender:    opts.Sender,
		store:     opts.Store,
		failed:    opts.Failed,
		location:  location,
		schedule:  schedule,
		peers:     opts.Peers,
		state:     state,
		urgentCh:  make(chan struct{}, 1),
		regularCh: make(chan drainSignal, 1),
		now:       nowFn,
	}

	logger.Debugf(
		"Queue: loaded state (regular=%d urgent=%d next_id=%d)",
		len(state.Regular), len(state.Urgent), state.NextID)

	return q, nil
}

// Start запускает воркера и планировщик; повторный вызов безопасно игнорируется (runOnce).
// При старте восстанавливает невыполненные urgent‑задачи и, если регулярное окно было пропущено
// (LastRegularDrainAt < предыдущий слот), инициирует дренирование сразу.
func (q *Queue) Start(ctx context.Context) {
	q.runOnce.Do(func() {
		// runOnce гарантирует, что очередь запустится только один раз даже при повторных вызовах.
		q.ctx, q.cancel = context.WithCancel(ctx)
		q.store.Start()
		if lifecycle, ok := q.sender.(interface{ Start(context.Context) }); ok {
			lifecycle.Start(q.ctx)
		}
		q.wg.Go(q.workerLoop)
		q.wg.Go(q.schedulerLoop)

		q.mu.Lock()
		hasUrgent := len(q.state.Urgent) > 0
		hasRegular := len(q.state.Regular) > 0
		lastDrain := q.state.LastRegularDrainAt
		q.mu.Unlock()

		if hasUrgent {
			logger.Infof("Queue: restoring %d urgent job(s) from disk", len(q.state.Urgent))
			q.signalUrgent()
		}
		if hasRegular {
			prevSlot := q.previousScheduleAt(q.now())
			if lastDrain.IsZero() || lastDrain.Before(prevSlot) {
				logger.Infof("Queue: missed window → draining")
				q.signalRegularDrain("startup missed window")
			}
		}
	})
}

// Run оставлен для обратной совместимости и просто вызывает Start().
func (q *Queue) Run(ctx context.Context) {
	q.Start(ctx)
}

// Close останавливает воркеры, вызывает Stop() у транспорта (если реализован),
// и форсирует Flush/Close у стора. Блокируется до завершения горутин или таймаута ctx.
func (q *Queue) Close(ctx context.Context) error {
	q.store.Start()
	if q.cancel != nil {
		q.cancel()
	}

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}

	if lifecycle, ok := q.sender.(interface{ Stop() }); ok {
		lifecycle.Stop()
	}

	if err := q.store.Flush(ctx); err != nil {
		logger.Errorf("Queue: flush error: %v", err)
		return err
	}
	if err := q.store.Close(ctx); err != nil {
		logger.Errorf("Queue: store close error: %v", err)
		return err
	}
	return nil
}

// Notify формирует задания из результата фильтра и ставит их в очередь.
// При флаге Forward добавляет спецификацию пересылки и, на всякий случай,
// подготовленную копию текста (для транспорта без пересылки).
func (q *Queue) Notify(entities tg.Entities, msg *tg.Message, fres filters.FilterMatchResult) error {
	if msg == nil {
		return errors.New("notifications queue: nil message")
	}

	link := BuildMessageLink(q.peers, entities, msg)
	text := RenderTemplate(fres.Filter.Notify.Template, fres.Result, link)

	payload := Payload{
		Text: strings.TrimSpace(text),
	}

	if fres.Filter.Notify.Forward {
		if fwd, err := buildForwardSpec(msg); err != nil {
			logger.Errorf("Queue: forward spec error for message %d: %v", msg.ID, err)
		} else {
			payload.Forward = fwd
		}
		// Если требуется «форвард», а бот не умеет пересылать — подготовим копию текста для Bot API.
		payload.Copy = BuildCopyTextFromTG(msg)
	}

	jobs := buildJobsFromTargets(fres.Filter.Notify.Recipients, fres.Filter.Urgent, payload)
	if len(jobs) == 0 {
		return errors.New("notifications queue: empty recipients list")
	}

	for _, job := range jobs {
		jobID := q.enqueue(job)
		logger.Debugf(
			"Queue: job %d enqueued (filter=%s urgent=%t recipient=%s:%d)",
			jobID, fres.Filter.ID, job.Urgent, job.Recipient.Type, job.Recipient.ID)
	}

	return nil
}

// Send ставит срочное задание на доставку произвольного текста конкретному пользователю.
func (q *Queue) Send(ctx context.Context, uid int64, text string) error {
	// Контекст не используется внутри: постановка выполняется синхронно и мгновенно.
	// Сохраняем подпись для совместимости с существующими вызовами.
	_ = ctx

	job := Job{
		Urgent: true,
		Recipient: Recipient{
			Type: RecipientTypeUser,
			ID:   uid,
		},
		Payload: Payload{
			Text: strings.TrimSpace(text),
		},
	}

	jobID := q.enqueue(job)
	logger.Debugf("Queue: direct job %d enqueued for user %d", jobID, uid)
	return nil
}

// enqueue присваивает job ID, сохраняет его в нужную очередь и планирует персист в фоне.
// Возвращает присвоенный идентификатор. Для urgent дополнительно сигналит воркеру.
func (q *Queue) enqueue(job Job) int64 {
	urgent := job.Urgent

	q.mu.Lock()

	job.ID = q.state.NextID
	job.CreatedAt = q.now().UTC()
	q.state.NextID++

	if job.Urgent {
		q.state.Urgent = append(q.state.Urgent, job)
	} else {
		q.state.Regular = append(q.state.Regular, job)
	}

	jobID := job.ID
	urgentLen := len(q.state.Urgent)
	regularLen := len(q.state.Regular)
	q.persistLocked()
	q.mu.Unlock()

	q.warnIfLarge(urgentLen, regularLen)

	if urgent {
		q.signalUrgent()
	}

	return jobID
}

// warnIfLarge логирует предупреждение при чрезмерном росте бэклогов.
func (q *Queue) warnIfLarge(urgentLen, regularLen int) {
	if urgentLen >= warnIfLargeSize {
		logger.Warnf("Queue: urgent backlog reached %d tasks", urgentLen)
	}
	if regularLen >= warnIfLargeSize {
		logger.Warnf("Queue: regular backlog reached %d tasks", regularLen)
	}
}

// Size возвращает текущие размеры urgent/regular бэклогов (без захвата снапшота состояния).
func (q *Queue) Size() (int, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.state.Urgent), len(q.state.Regular)
}

// HasPending сообщает, есть ли в очереди невыполненные задания любого типа.
func (q *Queue) HasPending() bool {
	u, r := q.Size()
	return u > 0 || r > 0
}

// Stats возвращает компактный снимок состояния очереди для CLI/мониторинга.
func (q *Queue) Stats() QueueStats {
	q.mu.Lock()
	urgent := len(q.state.Urgent)
	regular := len(q.state.Regular)
	lastDrain := q.state.LastRegularDrainAt
	lastFlush := q.state.LastFlushAt
	loc := q.location
	q.mu.Unlock()

	next := q.nextScheduleAfter(q.now())
	return QueueStats{
		Urgent:             urgent,
		Regular:            regular,
		LastRegularDrainAt: lastDrain,
		LastFlushAt:        lastFlush,
		NextScheduleAt:     next,
		Location:           loc,
	}
}

// workerLoop — главный цикл обработки сигналов. Приоритет: сначала завершение контекста,
// затем срочные задачи, затем регулярное дренирование.
func (q *Queue) workerLoop() {
	for {
		// Приоритет: сначала проверяем завершение контекста, затем срочные задачи,
		// и только после этого обрабатываем сигнал расписания.
		select {
		case <-q.ctx.Done():
			return
		case <-q.urgentCh:
			q.processUrgent()
		case signal := <-q.regularCh:
			q.processRegular(signal)
		}
	}
}

// triggerScheduledWindow дедуплицирует сигналы расписания по минутному окну и сигналит drain один раз за окно.
func (q *Queue) triggerScheduledWindow(win time.Time) {
	// Ключ окна: UTC, усечён до минутной гранулярности
	key := win.UTC().Truncate(scheduleKeyResolution).Unix()

	q.mu.Lock()
	if q.lastScheduledKey == key {
		q.mu.Unlock()
		return // Уже триггерили это окно — выходим без повторного сигнала/лога
	}
	q.lastScheduledKey = key
	q.mu.Unlock()

	logger.Debugf("Queue: schedule tick at %s", win.In(q.location).Format(time.RFC3339))
	q.signalRegularDrain("scheduled window")
}

// schedulerLoop ждёт наступления ближайшего слота расписания и триггерит дренирование.
func (q *Queue) schedulerLoop() {
	for {
		nextTick := q.nextScheduleAfter(q.now())
		// delay может быть отрицательным, если nextTick < now (например, при пропущенном слоте).
		delay := time.Until(nextTick)
		delay = max(delay, 0)

		timer := time.NewTimer(delay)
		select {
		case <-q.ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
			q.triggerScheduledWindow(nextTick)
		}
	}
}

// processUrgent дренирует срочную очередь до опустошения. Вызывает BeforeDrain у транспорта один раз.
func (q *Queue) processUrgent() {
	// Срочные задания обрабатываем до тех пор, пока в списке urgent есть элементы.
	// Если в процессе доставки появятся новые urgent-задачи, сигнал urgentCh
	// запустит цикл повторно.
	hookCalled := false
	for {
		job, hasUrgent := q.popUrgent()
		if !hasUrgent {
			return
		}
		if !hookCalled {
			if h, ok := q.sender.(interface{ BeforeDrain(context.Context) }); ok {
				h.BeforeDrain(q.ctx)
			}
			hookCalled = true
		}
		if q.handleJob(job) {
			return
		}
	}
}

// callBeforeDrainOnce вызывает BeforeDrain у sender ровно один раз за сессию дренирования.
func (q *Queue) callBeforeDrainOnce(called *bool) {
	if *called {
		return
	}
	if h, okSender := q.sender.(beforeDrainer); okSender {
		h.BeforeDrain(q.ctx)
	}
	*called = true
}

// drainUrgentOnce пытается обработать одно срочное задание перед каждым шагом регулярного дренирования.
// Возвращает interrupted=true, если обработка потребовала прерывания регулярного дренирования
// (requeue по сети/ctx), и processed=true, если срочное задание было и его попытались доставить.
func (q *Queue) drainUrgentOnce(reason string, hookCalled *bool) (bool, bool) {
	job, hasUrgent := q.popUrgent()
	if !hasUrgent {
		return false, false
	}
	q.callBeforeDrainOnce(hookCalled)
	if q.handleJob(job) {
		logger.Debugf("Queue: regular drain interrupted by urgent job (%s)", reason)
		return true, false
	}
	return false, true
}

// processRegular дренирует регулярную очередь, учитывая возможные прерывания срочными задачами.
// При полном опустошении фиксирует LastRegularDrainAt и синхронизирует состояние на диск.
func (q *Queue) processRegular(sig drainSignal) {
	reason := sig.reason
	logger.Debugf("Queue: start regular drain (%s)", reason)

	// drainedAll = true, если дошли до конца regular-очереди без прерываний
	drainedAll := false
	// Если пролог уже выполнен на продьюсер-пути, не дублируем.
	hookCalled := sig.preHookDone

	for {
		// Сначала пробуем обработать одно срочное задание, если есть.
		if interrupted, processed := q.drainUrgentOnce(reason, &hookCalled); interrupted {
			break
		} else if processed {
			continue
		}

		job, hasRegular := q.popRegular()
		if !hasRegular {
			// Регулярная очередь исчерпана — окно считаем обработанным
			drainedAll = true
			break
		}

		q.callBeforeDrainOnce(&hookCalled)

		if q.handleJob(job) {
			// Принудительное прерывание дренирования (requeue / offline / ctx)
			logger.Debugf("Queue: regular drain interrupted on job %d (%s)", job.ID, reason)
			break
		}
	}

	if drainedAll {
		q.mu.Lock()
		q.state.LastRegularDrainAt = q.now().UTC()
		q.persistLocked()
		q.mu.Unlock()
	}
	logger.Debugf("Queue: regular drain finished (%s)", reason)
}

// handleJob выполняет доставку одного задания и решает, нужно ли прервать текущую выборку.
// Возвращает true, если задание было возвращено в очередь или потребовалось ждать online/ctx.
func (q *Queue) handleJob(job Job) bool {
	start := q.now()
	logger.Debugf("Queue: delivering job %d (urgent=%t recipient=%s:%d)", job.ID, job.Urgent, job.Recipient.Type, job.Recipient.ID)

	ctx := q.ctx
	result, err := q.sender.Deliver(ctx, job)

	// Если transport сообщил, что соединение разорвано, возвращаем задание в начало очереди
	// и ждём, пока connection.WaitOnline не подтвердит восстановление.
	if result.NetworkDown {
		logger.Warnf("Queue: network offline, requeue job %d", job.ID)
		q.requeueJob(job, true)
		if ctxErr := ctx.Err(); ctxErr != nil {
			logger.Warnf("Queue: job %d waiting for connection but context already done: %v", job.ID, ctxErr)
		}
		connection.WaitOnline(ctx)
		return true
	}

	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			logger.Warnf("Queue: context canceled while delivering job %d, requeue", job.ID)
			q.requeueJob(job, true)
			return true
		}
		logger.Errorf("Queue: delivery error for job %d: %v", job.ID, err)
		q.requeueJob(job, false)
		return true
	}

	if result.Retry {
		logger.Warnf("Queue: sender requested retry for job %d", job.ID)
		q.requeueJob(job, false)
		return true
	}

	// Перманентные ошибки фиксируем в отдельном файле failed, чтобы оператор мог расследовать инцидент.
	if len(result.PermanentFailures) > 0 {
		errMsg := "permanent failure"
		if result.PermanentError != nil {
			errMsg = result.PermanentError.Error()
		}
		record := FailedRecord{
			Job:      job.Clone(),
			FailedAt: q.now().UTC(),
			Error:    errMsg,
		}
		if appendErr := q.failed.Append(record); appendErr != nil {
			logger.Errorf("Queue: failed store append error: %v", appendErr)
		}
		logger.Errorf(
			"Queue: job %d permanent failure for recipient %s:%d: %s",
			job.ID, job.Recipient.Type, job.Recipient.ID, errMsg)
	}

	duration := time.Since(start)
	logger.Debugf("Queue: job %d processed in %s", job.ID, duration)
	return false
}

// requeueJob возвращает задание обратно в соответствующую очередь. front=true — поставить в начало.
func (q *Queue) requeueJob(job Job, front bool) {
	q.mu.Lock()

	state := &q.state.Regular
	if job.Urgent {
		state = &q.state.Urgent
	}

	if front {
		clone := job.Clone()
		*state = append([]Job{clone}, *state...)
	} else {
		*state = append(*state, job.Clone())
	}

	q.persistLocked()
	q.mu.Unlock()

	if job.Urgent {
		q.signalUrgent()
	} else if front {
		q.signalRegularDrain("connection recovery")
	}
}

// popUrgent снимает первое срочное задание, обновляет состояние и планирует persist.
func (q *Queue) popUrgent() (Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.state.Urgent) == 0 {
		return Job{}, false
	}
	job := q.state.Urgent[0]
	q.state.Urgent = q.state.Urgent[1:]
	q.persistLocked()
	return job, true
}

// popRegular снимает первое регулярное задание, обновляет состояние и планирует persist.
func (q *Queue) popRegular() (Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.state.Regular) == 0 {
		return Job{}, false
	}
	job := q.state.Regular[0]
	q.state.Regular = q.state.Regular[1:]
	q.persistLocked()
	return job, true
}

// persistLocked помечает время последней синхронизации и планирует запись состояния (без блокировки диска здесь).
func (q *Queue) persistLocked() {
	q.state.LastFlushAt = q.now().UTC()
	q.store.SchedulePersist(q.state.Clone())
}

// signalUrgent пробует неблокирующе уведомить воркер о наличии срочных задач.
func (q *Queue) signalUrgent() {
	select {
	case q.urgentCh <- struct{}{}:
	default:
	}
}

// signalRegularDrain отправляет неблокирующий сигнал на дренирование регулярной очереди.
func (q *Queue) signalRegularDrain(reason string) {
	// Выполним «человечный» пролог на продьюсер-пути (только для транспортов, которые его поддерживают).
	if h, ok := q.sender.(interface{ BeforeDrain(context.Context) }); ok {
		h.BeforeDrain(q.ctx)
	}
	req := drainSignal{reason: reason, preHookDone: true}
	select {
	case q.regularCh <- req:
	default:
	}
}

// FlushImmediately инициирует внеплановый слив регулярной очереди из CLI/оператора (неблокирующе).
func (q *Queue) FlushImmediately(reason string) {
	if reason == "" {
		reason = "manual flush"
	}
	q.signalRegularDrain(reason)
}

// nextScheduleAfter вычисляет следующий слот расписания в локальной таймзоне и возвращает его в UTC.
func (q *Queue) nextScheduleAfter(now time.Time) time.Time {
	localNow := now.In(q.location)
	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, q.location)

	for _, entry := range q.schedule {
		slot := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), entry.hour, entry.minute, 0, 0, q.location)
		if slot.After(localNow) {
			return slot.UTC()
		}
	}

	// все слоты прошли → берём первое время следующего дня
	first := q.schedule[0]
	nextDay := today.Add(24 * time.Hour) //nolint: mnd // next day
	next := time.Date(nextDay.Year(), nextDay.Month(), nextDay.Day(), first.hour, first.minute, 0, 0, q.location)
	return next.UTC()
}

// previousScheduleAt возвращает предыдущий слот расписания относительно now в локальной таймзоне (результат в UTC).
func (q *Queue) previousScheduleAt(now time.Time) time.Time {
	localNow := now.In(q.location)
	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, q.location)

	// Идём с конца, чтобы найти ближайший slot <= now в сегодняшнем дне
	for i := len(q.schedule) - 1; i >= 0; i-- {
		entry := q.schedule[i]
		slot := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), entry.hour, entry.minute, 0, 0, q.location)
		if slot.Before(localNow) {
			return slot.UTC()
		}
	}

	// Нет слотов ранее в текущий день → берём последний слот вчера
	last := q.schedule[len(q.schedule)-1]
	yesterday := today.Add(-24 * time.Hour)
	prev := time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), last.hour, last.minute, 0, 0, q.location)
	return prev.UTC()
}

// buildJobsFromTargets создает список Job из targets, по одной на каждого получателя.
func buildJobsFromTargets(t recipients.RecipientTargets, urgent bool, payload Payload) []Job {
	total := len(t.Users) + len(t.Chats) + len(t.Channels)
	if total == 0 {
		return nil
	}
	out := make([]Job, 0, total)
	seen := make(map[string]struct{}, total)

	add := func(kind string, ids []int64) {
		for _, id := range ids {
			if id == 0 {
				continue
			}
			key := kind + ":" + strconv.FormatInt(id, 10)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			job := Job{
				Urgent:    urgent,
				Recipient: Recipient{Type: kind, ID: id},
				Payload:   payload,
			}
			out = append(out, job)
		}
	}

	add(RecipientTypeUser, t.Users)
	add(RecipientTypeChat, t.Chats)
	add(RecipientTypeChannel, t.Channels)

	return out
}

// buildForwardSpec готовит спецификацию пересылки исходного сообщения.
func buildForwardSpec(msg *tg.Message) (*ForwardSpec, error) {
	if msg == nil {
		return nil, errors.New("message is nil")
	}
	fromPeer, err := peerToRecipient(msg.PeerID)
	if err != nil {
		return nil, err
	}
	return &ForwardSpec{
		Enabled:    true,
		FromPeer:   fromPeer,
		MessageIDs: []int{msg.ID},
	}, nil
}

// peerToRecipient преобразует tg.PeerClass в доменную сущность Recipient.
func peerToRecipient(peer tg.PeerClass) (Recipient, error) {
	switch v := peer.(type) {
	case *tg.PeerUser:
		return Recipient{Type: RecipientTypeUser, ID: v.UserID}, nil
	case *tg.PeerChat:
		return Recipient{Type: RecipientTypeChat, ID: v.ChatID}, nil
	case *tg.PeerChannel:
		return Recipient{Type: RecipientTypeChannel, ID: v.ChannelID}, nil
	default:
		return Recipient{}, fmt.Errorf("unsupported peer type %T", peer)
	}
}

// parseSchedule принимает список токенов "HH:MM", валидирует диапазоны и сортирует слоты по времени.
func parseSchedule(raw []string) ([]scheduleEntry, error) {
	entries := make([]scheduleEntry, 0, len(raw))
	for _, token := range raw {
		parts := strings.Split(token, ":")
		if len(parts) != 2 { //nolint: mnd // why not?
			return nil, fmt.Errorf("invalid schedule token %q", token)
		}
		hour, err := strconvAtoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid hour in %q: %w", token, err)
		}
		minute, err := strconvAtoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("invalid minute in %q: %w", token, err)
		}
		if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
			return nil, fmt.Errorf("schedule %q out of range", token)
		}
		entries = append(entries, scheduleEntry{
			hour:   hour,
			minute: minute,
			label:  token,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].hour == entries[j].hour {
			return entries[i].minute < entries[j].minute
		}
		return entries[i].hour < entries[j].hour
	})

	return entries, nil
}

// strconvAtoi — Atoi с защитой от пустых строк и лишних пробелов.
func strconvAtoi(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("empty value")
	}
	return strconv.Atoi(value)
}
