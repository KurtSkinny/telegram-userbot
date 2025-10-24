// Package notifications — доменные модели очереди уведомлений.
// Здесь описаны получатели, полезная нагрузка, спецификация пересылки и сериализуемые
// состояния очереди. Также приведены безопасные функции клонирования, чтобы снапшоты,
// попавшие в персист, не зависели от дальнейших мутаций в рантайме.

package notifications

import "time"

// Recipient описывает получателя уведомления: тип peer и его числовой идентификатор.
// Тип хранится как человекочитаемая строка (user/chat/channel), чтобы JSON был стабилен,
// а транспорт мог по ней выбрать корректный способ резолва InputPeer.
type Recipient struct {
	Type string `json:"type"`
	ID   int64  `json:"id"`
}

// Типы получателей. Держим строковые метки стабильными, так как они попадают в персист.
const (
	// RecipientTypeUser указывает, что уведомление предназначено конкретному пользователю.
	RecipientTypeUser = "user"
	// RecipientTypeChat означает групповую беседу (legacy-chats и супергруппы без channel-like API).
	RecipientTypeChat = "chat"
	// RecipientTypeChannel маркирует каналы и мегагруппы, требующие InputPeerChannel.
	RecipientTypeChannel = "channel"
)

// ForwardSpec описывает пересылку оригинального сообщения вместе с уведомлением.
// Enabled оставлен явным, чтобы отличать «пересылка выключена» от «поле отсутствует в JSON».
// Если транспорт не умеет пересылку, очередь может сформировать CopyText (см. Payload.Copy).
type ForwardSpec struct {
	Enabled    bool      `json:"enabled"`
	FromPeer   Recipient `json:"from_peer"`
	MessageIDs []int     `json:"message_ids"`
}

// Payload содержит финальный текст уведомления и опциональную спецификацию пересылки/копии.
// Текст уже отрендерен по шаблону и не требует постобработки у транспорта.
// Поле Copy используется, когда пересылка недоступна или нежелательна; тип CopyText определяется в пакете отправителя.
type Payload struct {
	Text    string       `json:"text"`
	Forward *ForwardSpec `json:"forward,omitempty"`
	Copy    *CopyText    `json:"copy,omitempty"`
}

// Job — единица работы очереди уведомлений. Один job может адресоваться нескольким получателям.
// Идентификатор ID монотонно растёт и используется, среди прочего, для детерминированного random_id.
// Порядок доставки получателям — FIFO.
type Job struct {
	ID         int64       `json:"id"`
	CreatedAt  time.Time   `json:"created_at"`
	Urgent     bool        `json:"urgent"`
	Recipients []Recipient `json:"recipients"`
	Payload    Payload     `json:"payload"`
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

// FailedRecord фиксирует окончательно провалившуюся доставку: полный снимок job,
// конкретных недоставленных получателей и текст агрегированной ошибки.
type FailedRecord struct {
	Job        Job         `json:"job"`
	FailedAt   time.Time   `json:"failed_at"`
	Error      string      `json:"error"`
	Recipients []Recipient `json:"recipients"`
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
	clone.Recipients = cloneRecipients(j.Recipients)
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

// Clone возвращает независимую копию записи о провале, включая вложенный Job и список получателей.
func (r FailedRecord) Clone() FailedRecord {
	clone := r
	clone.Job = r.Job.Clone()
	clone.Recipients = cloneRecipients(r.Recipients)
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

// cloneRecipients делает поверхностную копию. Достаточно copy, так как Recipient — value-тип без вложенных указателей.
func cloneRecipients(in []Recipient) []Recipient {
	if len(in) == 0 {
		return nil
	}
	out := make([]Recipient, len(in))
	copy(out, in)
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
