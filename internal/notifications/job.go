// Package notifications — доменные модели очереди уведомлений.
// Здесь описаны получатели, полезная нагрузка, спецификация пересылки и сериализуемые
// состояния очереди. Также приведены безопасные функции клонирования, чтобы снапшоты,
// попавшие в персист, не зависели от дальнейших мутаций в рантайме.

package notifications

import (
	"time"

	"telegram-userbot/internal/filters"
)

// ForwardSpec описывает пересылку оригинального сообщения вместе с уведомлением.
// Enabled оставлен явным, чтобы отличать «пересылка выключена» от «поле отсутствует в JSON».
// Если транспорт не умеет пересылку, очередь может сформировать CopyText (см. Payload.Copy).
type ForwardSpec struct {
	Enabled    bool              `json:"enabled"`
	FromPeer   filters.Recipient `json:"from_peer"`
	MessageIDs []int             `json:"message_ids"`
}

// Payload содержит финальный текст уведомления и опциональную спецификацию пересылки/копии.
// Текст уже отрендерен по шаблону и не требует постобработки у транспорта.
// Поле Copy используется, когда пересылка недоступна или нежелательна; тип CopyText определяется в пакете отправителя.
type Payload struct {
	Text    string       `json:"text"`
	Forward *ForwardSpec `json:"forward,omitempty"`
	Copy    *CopyText    `json:"copy,omitempty"`
}

// Job — единица работы очереди уведомлений. Один job адресуется одному получателю.
// Идентификатор ID монотонно растёт и используется, среди прочего, для детерминированного random_id.
// Порядок доставки получателям — FIFO.
// Теперь использует filters.Recipient с полной информацией о TZ и Schedule.
type Job struct {
	ID          int64             `json:"id"`
	CreatedAt   time.Time         `json:"created_at"`
	ScheduledAt time.Time         `json:"scheduled_at"` // Время запланированной отправки в UTC с учетом персональных настроек получателя
	Urgent      bool              `json:"urgent"`
	Recipient   filters.Recipient `json:"recipient"` // Полная информация о получателе включая TZ и Schedule
	Payload     Payload           `json:"payload"`
}

// State — сериализуемый снимок очереди: бэклоги urgent/regular, счётчик NextID и метки времени.
// LastRegularDrainAt помогает определить пропущенное окно расписания после рестарта.
// Все времени хранятся в UTC.
type State struct {
	LastFlushAt        time.Time `json:"last_flush_at"`
	LastRegularDrainAt time.Time `json:"last_regular_drain_at"`
	NextID             int64     `json:"next_id"`
	Regular            []Job     `json:"regular"`
	Urgent             []Job     `json:"urgent"`
}

// FailedRecord фиксирует окончательно провалившуюся доставку: полный снимок job
// и текст агрегированной ошибки.
type FailedRecord struct {
	Job      Job       `json:"job"`
	FailedAt time.Time `json:"failed_at"`
	Error    string    `json:"error"`
}

// DefaultState создаёт начальное состояние очереди: NextID=1, пустые бэклоги, нулевые метки времени.
func DefaultState() State {
	return State{
		NextID: 1,
	}
}

// Clone делает глубокую копию Job, чтобы дальнейшие изменения исходника не затронули персист/бэклоги.
func (j Job) Clone() Job {
	clone := j
	clone.Payload = clonePayload(j.Payload)
	return clone
}

// Clone создаёт глубокую копию State, включая срезы urgent/regular (с сохранением порядка).
func (s State) Clone() State {
	clone := s
	clone.Regular = cloneJobs(s.Regular)
	clone.Urgent = cloneJobs(s.Urgent)
	return clone
}

// Clone возвращает независимую копию записи о провале, включая вложенный Job.
func (r FailedRecord) Clone() FailedRecord {
	clone := r
	clone.Job = r.Job.Clone()
	return clone
}

// cloneJobs минимизирует аллокации: заранее выделяет результат нужной длины и копирует элементы через Clone.
func cloneJobs(in []Job) []Job {
	if len(in) == 0 {
		return nil
	}
	out := make([]Job, len(in))
	for i, job := range in {
		out[i] = job.Clone()
	}
	return out
}

// clonePayload копирует полезную нагрузку, включая ForwardSpec, если она присутствует.
func clonePayload(in Payload) Payload {
	clone := in
	if in.Forward != nil {
		clone.Forward = cloneForwardSpec(in.Forward)
	}
	return clone
}

// cloneForwardSpec делает независимую копию ForwardSpec и её MessageIDs (через append к nil-срезу).
func cloneForwardSpec(in *ForwardSpec) *ForwardSpec {
	if in == nil {
		return nil
	}
	clone := *in
	clone.MessageIDs = append([]int(nil), in.MessageIDs...)
	return &clone
}
