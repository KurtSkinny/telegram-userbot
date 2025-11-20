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
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/telegram/peersmgr"

	"github.com/gotd/td/tg"
)

const warnIfLargeSize = 1000

type PreparedSender interface {
	Deliver(ctx context.Context, job Job) (SendOutcome, error)
}

type SendOutcome struct {
	PermanentFailures []filters.Recipient
	PermanentError    error
	NetworkDown       bool
	Retry             bool
}

type QueueOptions struct {
	Sender   PreparedSender
	Store    *QueueStore
	Failed   *FailedStore
	Schedule []string
	Location *time.Location
	Clock    func() time.Time
	Peers    *peersmgr.Service
}

type scheduleEntry struct {
	hour   int
	minute int
	label  string
}

type beforeDrainer interface {
	BeforeDrain(context.Context)
}

type QueueStats struct {
	Urgent             int
	Regular            int
	LastRegularDrainAt time.Time
	LastFlushAt        time.Time
	NextScheduleAt     time.Time
	Location           *time.Location
}

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
	regularCh chan string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	now     func() time.Time
	runOnce sync.Once
}

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

	state.Urgent = filterValidJobs(state.Urgent)
	state.Regular = filterValidJobs(state.Regular)

	q := &Queue{
		sender:    opts.Sender,
		store:     opts.Store,
		failed:    opts.Failed,
		location:  location,
		schedule:  schedule,
		peers:     opts.Peers,
		state:     state,
		urgentCh:  make(chan struct{}, 1),
		regularCh: make(chan string, 1),
		now:       nowFn,
	}

	logger.Debugf(
		"Queue: loaded state (regular=%d urgent=%d next_id=%d)",
		len(state.Regular), len(state.Urgent), state.NextID)

	return q, nil
}

func filterValidJobs(jobs []Job) []Job {
	n := 0
	for _, job := range jobs {
		if job.Recipient.Type != "" && job.Recipient.PeerID != 0 {
			jobs[n] = job
			n++
		} else {
			logger.Errorf("Queue: skipping invalid job %d (empty recipient)", job.ID)
		}
	}
	return jobs[:n]
}

func (q *Queue) Start(ctx context.Context) {
	q.runOnce.Do(func() {
		q.ctx, q.cancel = context.WithCancel(ctx)
		q.store.Start()
		q.wg.Go(q.workerLoop)
		q.wg.Go(q.schedulerLoop)

		q.mu.Lock()
		hasUrgent := len(q.state.Urgent) > 0
		q.mu.Unlock()

		if hasUrgent {
			logger.Infof("Queue: restoring %d urgent job(s) from disk", len(q.state.Urgent))
			q.signalUrgent()
		}
	})
}

func (q *Queue) Stop() error {
	if q.cancel != nil {
		q.cancel()
	}

	q.wg.Wait()
	return q.store.Stop()
}

func (q *Queue) Notify(entities tg.Entities, msg *tg.Message, fres filters.FilterMatchResult) error {
	if msg == nil {
		return errors.New("notifications queue: nil message")
	}

	link := BuildMessageLink(q.peers, entities, msg)
	text := RenderTemplate(fres.Filter.Notify.Template, fres.Result, link)

	payload := Payload{Text: strings.TrimSpace(text)}

	if fres.Filter.Notify.Forward {
		if fwd, err := buildForwardSpec(msg); err != nil {
			logger.Errorf("Queue: forward spec error for message %d: %v", msg.ID, err)
		} else {
			payload.Forward = fwd
		}
		payload.Copy = BuildCopyTextFromTG(msg)
	}

	for _, r := range fres.Recipients {
		job := Job{
			Urgent:    fres.Filter.Notify.Urgent,
			Recipient: r,
			Payload:   payload,
		}
		jobID := q.enqueue(job)
		logger.Debugf(
			"Queue: job %d enqueued (filter=%s urgent=%t recipient=%s:%d)",
			jobID, fres.Filter.ID, job.Urgent, job.Recipient.Type, job.Recipient.PeerID)
	}

	return nil
}

func (q *Queue) enqueue(job Job) int64 {
	now := q.now()

	q.mu.Lock()

	job.ID = q.state.NextID
	job.CreatedAt = now.UTC()

	defaultSchedule := make([]string, len(q.schedule))
	for i, entry := range q.schedule {
		defaultSchedule[i] = entry.label
	}
	job.ScheduledAt = job.Recipient.CalculateScheduledTime(job.Urgent, now, q.location, defaultSchedule)

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

	logger.Debugf("Queue: job %d scheduled for %s UTC (recipient=%s:%d, urgent=%t, tz=%s)",
		jobID, job.ScheduledAt.Format(time.RFC3339), job.Recipient.Type, job.Recipient.PeerID,
		job.Urgent, job.Recipient.TZ)

	q.warnIfLarge(urgentLen, regularLen)

	if job.Urgent {
		q.signalUrgent()
	}

	return jobID
}

func (q *Queue) warnIfLarge(urgentLen, regularLen int) {
	if urgentLen >= warnIfLargeSize {
		logger.Warnf("Queue: urgent backlog reached %d tasks", urgentLen)
	}
	if regularLen >= warnIfLargeSize {
		logger.Warnf("Queue: regular backlog reached %d tasks", regularLen)
	}
}

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

func (q *Queue) workerLoop() {
	defer logger.Debug("Queue: worker loop exited")
	for {
		select {
		case <-q.ctx.Done():
			return
		case <-q.urgentCh:
			q.processUrgent()
		case reason := <-q.regularCh:
			q.processRegular(reason)
		}
	}
}

func (q *Queue) hasReadyJobs() bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := q.now()
	for _, job := range q.state.Regular {
		if !job.ScheduledAt.After(now) {
			return true
		}
	}
	return false
}

func (q *Queue) schedulerLoop() {
	defer logger.Debug("Queue: scheduler loop exited")
	for {
		if q.hasReadyJobs() {
			q.signalRegularDrain("scheduled jobs ready")
		}

		now := q.now()
		nextMinute := now.Truncate(time.Minute).Add(time.Minute)
		sleepDuration := nextMinute.Sub(now)

		timer := time.NewTimer(sleepDuration)

		select {
		case <-q.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (q *Queue) processUrgent() {
	if h, ok := q.sender.(beforeDrainer); ok {
		h.BeforeDrain(q.ctx)
	}
	for {
		job, hasJob := q.popUrgent()
		if !hasJob {
			return
		}
		if q.handleJob(job) {
			return
		}
	}
}

func (q *Queue) processRegular(reason string) {
	logger.Debugf("Queue: start regular drain (%s)", reason)
	if h, ok := q.sender.(beforeDrainer); ok {
		h.BeforeDrain(q.ctx)
	}

	drainedAll := true
	for {
		job, hasJob := q.popRegular()
		if !hasJob {
			break
		}
		if q.handleJob(job) {
			drainedAll = false
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

func (q *Queue) handleJob(job Job) bool {
	start := q.now()
	logger.Debugf("Queue: delivering job %d (urgent=%t recipient=%s:%d)",
		job.ID, job.Urgent, job.Recipient.Type, job.Recipient.PeerID)

	result, err := q.sender.Deliver(q.ctx, job)

	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			logger.Warnf("Queue: context canceled, requeue job %d", job.ID)
			q.requeueJob(job, true)
			return true
		}
		logger.Errorf("Queue: delivery error for job %d: %v", job.ID, err)
		q.requeueJob(job, false)
		return true
	}

	if result.NetworkDown {
		logger.Warnf("Queue: network offline, requeue job %d", job.ID)
		q.requeueJob(job, true)
		return true
	}

	if result.Retry {
		logger.Warnf("Queue: sender requested retry for job %d", job.ID)
		q.requeueJob(job, false)
		return true
	}

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
			job.ID, job.Recipient.Type, job.Recipient.PeerID, errMsg)
	}

	duration := time.Since(start)
	logger.Debugf("Queue: job %d processed in %s", job.ID, duration)
	return false
}

func (q *Queue) requeueJob(job Job, front bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	state := &q.state.Regular
	if job.Urgent {
		state = &q.state.Urgent
	}

	clone := job.Clone()

	if front {
		*state = append([]Job{clone}, *state...)
	} else {
		*state = append(*state, clone)
	}

	q.persistLocked()

	if job.Urgent {
		q.signalUrgent()
	}
}

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

func (q *Queue) popRegular() (Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := q.now()
	for i, job := range q.state.Regular {
		if !job.ScheduledAt.After(now) {
			q.state.Regular = append(q.state.Regular[:i], q.state.Regular[i+1:]...)
			q.persistLocked()
			return job, true
		}
	}

	return Job{}, false
}

func (q *Queue) persistLocked() {
	q.state.LastFlushAt = q.now().UTC()
	q.store.SchedulePersist(q.state.Clone())
}

func (q *Queue) signalUrgent() {
	select {
	case q.urgentCh <- struct{}{}:
	default:
	}
}

func (q *Queue) signalRegularDrain(reason string) {
	select {
	case q.regularCh <- reason:
	default:
	}
}

func (q *Queue) FlushImmediately(reason string) {
	if reason == "" {
		reason = "manual flush"
	}
	logger.Debugf("Queue: force flush requested (%s)", reason)
	
	// Принудительно обновляем ScheduledAt всех regular заданий на текущее время
	// чтобы они были обработаны немедленно
	q.mu.Lock()
	now := q.now()
	for i := range q.state.Regular {
		q.state.Regular[i].ScheduledAt = now.Add(-time.Second) // в прошлом, чтобы гарантированно пройти проверку
	}
	q.persistLocked()
	q.mu.Unlock()
	
	q.signalRegularDrain(reason)
}

func (q *Queue) nextScheduleAfter(now time.Time) time.Time {
	if len(q.schedule) == 0 {
		return now.Add(time.Hour)
	}

	localNow := now.In(q.location)
	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, q.location)

	for _, entry := range q.schedule {
		slot := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), entry.hour, entry.minute, 0, 0, q.location)
		if slot.After(localNow) {
			return slot.UTC()
		}
	}

	first := q.schedule[0]
	const dayDuration = 24 * time.Hour
	nextDay := today.Add(dayDuration)
	return time.Date(nextDay.Year(), nextDay.Month(), nextDay.Day(), first.hour, first.minute, 0, 0, q.location).UTC()
}

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

func peerToRecipient(peer tg.PeerClass) (filters.Recipient, error) {
	switch v := peer.(type) {
	case *tg.PeerUser:
		return filters.Recipient{Type: filters.RecipientTypeUser, PeerID: filters.RecipientPeerID(v.UserID)}, nil
	case *tg.PeerChat:
		return filters.Recipient{Type: filters.RecipientTypeChat, PeerID: filters.RecipientPeerID(v.ChatID)}, nil
	case *tg.PeerChannel:
		return filters.Recipient{Type: filters.RecipientTypeChannel, PeerID: filters.RecipientPeerID(v.ChannelID)}, nil
	default:
		return filters.Recipient{}, fmt.Errorf("unsupported peer type %T", peer)
	}
}

func parseSchedule(raw []string) ([]scheduleEntry, error) {
	entries := make([]scheduleEntry, 0, len(raw))
	for _, token := range raw {
		parts := strings.Split(token, ":")
		const expectedParts = 2
		if len(parts) != expectedParts {
			return nil, fmt.Errorf("invalid schedule token %q", token)
		}
		hour, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid hour in %q: %w", token, err)
		}
		minute, err := strconv.Atoi(parts[1])
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
