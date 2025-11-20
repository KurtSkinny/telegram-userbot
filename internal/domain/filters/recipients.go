// recipients.go содержит структуры и методы для загрузки, хранения и управления
// получателями сообщений.
package filters

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"telegram-userbot/internal/infra/timeutil"
)

type RecipientID string

func (rid *RecipientID) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("recipient ID cannot be empty")
	}
	*rid = RecipientID(s)
	return nil
}

// RecipientType определяет тип получателя: пользователь, чат или канал
//
//nolint:recvcheck // UnmarshalJSON requires pointer receiver, others use value receiver
type RecipientType string

const (
	RecipientTypeUser    RecipientType = "user"
	RecipientTypeChat    RecipientType = "chat"
	RecipientTypeChannel RecipientType = "channel"
)

// IsValid проверяет, что RecipientType имеет допустимое значение
func (rt RecipientType) IsValid() bool {
	switch rt {
	case RecipientTypeUser, RecipientTypeChat, RecipientTypeChannel:
		return true
	default:
		return false
	}
}

// String возвращает строковое представление RecipientType
func (rt RecipientType) String() string {
	return string(rt)
}

// UnmarshalJSON реализует проверку на допустимые значения RecipientType
func (rt *RecipientType) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}

	t := RecipientType(s)
	if !t.IsValid() {
		return fmt.Errorf("invalid recipient type: %s", s)
	}

	*rt = t
	return nil
}

type RecipientPeerID int64

func (rp *RecipientPeerID) UnmarshalJSON(data []byte) error {
	var id int64
	if err := json.Unmarshal(data, &id); err != nil {
		return err
	}
	if id <= 0 {
		return errors.New("peer_id must be a positive integer")
	}
	*rp = RecipientPeerID(id)
	return nil
}

type RecipientTZ string

// UnmarshalJSON реализует кастомную десериализацию для RecipientTZ
func (rt *RecipientTZ) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s != "" {
		if _, err := timeutil.ParseLocation(s); err != nil {
			return fmt.Errorf("invalid timezone: %s", s)
		}
	}
	*rt = RecipientTZ(s)
	return nil
}

type RecipientSchedule string

// UnmarshalJSON реализует проверку на валидность формата HH:MM
func (rs *RecipientSchedule) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s != "" && !timeutil.IsValidScheduleEntry(s) {
		return fmt.Errorf("invalid schedule entry: %s", s)
	}
	*rs = RecipientSchedule(s)
	return nil
}

// Recipient — полное описание получателя из recipients.json
type Recipient struct {
	ID       RecipientID         `json:"id"`       // Уникальный ID получателя (обяазателен)
	Type     RecipientType       `json:"type"`     // user|chat|channel (обязателен)
	PeerID   RecipientPeerID     `json:"peer_id"`  // Telegram peer_id (обязателен)
	Note     string              `json:"note"`     // Заметка для получателя (необязательна)
	TZ       RecipientTZ         `json:"tz"`       // IANA-таймзона или UTC-смещение (необязательна)
	Schedule []RecipientSchedule `json:"schedule"` // Строка с расписанием в формате HH:MM[,HH:MM,...] (необязательна)
}

// UnmarshalJSON проверка обязательных полей Recipient
func (r *Recipient) UnmarshalJSON(data []byte) error {
	type Alias Recipient
	var aux Alias
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.ID == "" {
		return errors.New("recipient ID is required")
	}
	if aux.Type == "" {
		return errors.New("recipient type is required")
	}
	if aux.PeerID == 0 {
		return errors.New("recipient peer_id is required")
	}
	*r = Recipient(aux)
	return nil
}

// LoadRecipients читает, парсит JSON-файл с получателями и возвращает срез Recipient.
func LoadRecipients(filePath string) ([]Recipient, error) {
	data, readErr := os.ReadFile(filepath.Clean(filePath))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read recipients json: %w", readErr)
	}

	var recipients []Recipient
	if err := json.Unmarshal(data, &recipients); err != nil {
		return nil, fmt.Errorf("failed to unmarshal recipients json: %w", err)
	}

	return recipients, nil
}

// CalculateScheduledTime вычисляет время отправки для получателя с учетом его персональных настроек.
// Для срочных заданий возвращает текущее время. Для обычных - следующий подходящий слот расписания.
func (r *Recipient) CalculateScheduledTime(
	urgent bool,
	now time.Time,
	defaultTZ *time.Location,
	defaultSchedule []string,
) time.Time {
	if urgent {
		return now.UTC() // Срочные задания отправляются немедленно
	}

	// Определяем эффективную таймзону
	tz := defaultTZ
	if r.TZ != "" {
		if loc, err := timeutil.ParseLocation(string(r.TZ)); err == nil {
			tz = loc
		}
	}

	// Определяем эффективное расписание
	schedule := defaultSchedule
	if len(r.Schedule) > 0 {
		schedule = make([]string, len(r.Schedule))
		for i, s := range r.Schedule {
			schedule[i] = string(s)
		}
	}

	return nextScheduleAfter(now, tz, schedule).UTC()
}

// nextScheduleAfter вычисляет следующий слот расписания после указанного времени.
func nextScheduleAfter(now time.Time, location *time.Location, schedule []string) time.Time {
	localNow := now.In(location)
	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location)

	// Ищем ближайший слот сегодня
	for _, timeStr := range schedule {
		if !timeutil.IsValidScheduleEntry(timeStr) {
			continue
		}
		parts := strings.Split(timeStr, ":")
		hour, _ := strconv.Atoi(parts[0])
		minute, _ := strconv.Atoi(parts[1])

		slot := time.Date(localNow.Year(), localNow.Month(), localNow.Day(),
			hour, minute, 0, 0, location)
		if slot.After(localNow) {
			return slot
		}
	}

	const hoursInDay = 24

	// Все слоты прошли - берем первый слот следующего дня
	if len(schedule) > 0 {
		timeStr := schedule[0]
		if timeutil.IsValidScheduleEntry(timeStr) {
			parts := strings.Split(timeStr, ":")
			hour, _ := strconv.Atoi(parts[0])
			minute, _ := strconv.Atoi(parts[1])

			nextDay := today.Add(hoursInDay * time.Hour)
			next := time.Date(nextDay.Year(), nextDay.Month(), nextDay.Day(),
				hour, minute, 0, 0, location)
			return next
		}
	}

	// Fallback - если расписание пустое, планируем на завтра в это же время
	return localNow.Add(hoursInDay * time.Hour)
}
