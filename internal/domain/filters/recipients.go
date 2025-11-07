// recipients.go содержит структуры и методы для загрузки, хранения и управления
// получателями сообщений.
package filters

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
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
type RecipientType string

const (
	RecipientTypeUser    RecipientType = "user"
	RecipientTypeChat    RecipientType = "chat"
	RecipientTypeChannel RecipientType = "channel"
)

// IsValid проверяет, что RecipientType имеет допустимое значение
func (rt *RecipientType) IsValid() bool {
	switch *rt {
	case RecipientTypeUser, RecipientTypeChat, RecipientTypeChannel:
		return true
	default:
		return false
	}
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
		if _, err := ParseLocation(s); err != nil {
			return fmt.Errorf("invalid timezone: %s", s)
		}
	}
	*rt = RecipientTZ(s)
	return nil
}

type RecipientSchedule struct {
	Hour   int
	Minute int
	Label  string
}

// UnmarshalJSON реализует кастомную десериализацию для RecipientSchedule
func (rs *RecipientSchedule) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		*rs = RecipientSchedule{}
		return nil
	}
	if !IsValidScheduleEntry(s) {
		return fmt.Errorf("invalid schedule entry: %s", s)
	}

	parts := strings.Split(s, ":")
	hour, _ := strconv.Atoi(parts[0])
	minute, _ := strconv.Atoi(parts[1])
	*rs = RecipientSchedule{
		Hour:   hour,
		Minute: minute,
		Label:  s,
	}
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

// IsValidScheduleEntry проверяет формат времени HH:MM и диапазоны часов/минут.
// Это копия функции из config пакета
func IsValidScheduleEntry(value string) bool {
	if len(value) != 5 || value[2] != ':' {
		return false
	}
	hour, err := strconv.Atoi(value[:2])
	if err != nil {
		return false
	}
	minute, err := strconv.Atoi(value[3:])
	if err != nil {
		return false
	}
	if hour < 0 || hour > 23 {
		return false
	}
	if minute < 0 || minute > 59 {
		return false
	}
	return true
}

// ParseLocation разбирает либо IANA‑таймзону (например, "Europe/Moscow"),
// либо UTC‑смещение (например, "+03:00", "-0700", "UTC+3", "GMT-04:30").
// Это копия функции из config пакета
func ParseLocation(value string) (*time.Location, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil, errors.New("empty timezone")
	}
	// Try IANA first.
	if loc, err := time.LoadLocation(v); err == nil {
		return loc, nil
	}
	// Try to parse UTC offset forms.
	if loc, ok := parseUTCOffsetToLocation(v); ok {
		return loc, nil
	}
	return nil, fmt.Errorf("invalid timezone %q: not an IANA name or UTC offset", value)
}

// constants for time calculations
const (
	secondsInMinute = 60
	secondsInHour   = secondsInMinute * 60
)

// parseUTCOffsetToLocation парсит строки вида "+03:00", "-0700", "UTC+3", "GMT-04:30" или "Z".
// Возвращает фиксированную таймзону и ok=true при успешном разборе.
// Это копия функции из config пакета
func parseUTCOffsetToLocation(value string) (*time.Location, bool) {
	v := strings.TrimSpace(strings.ToUpper(value))
	if v == "Z" || v == "UTC" || v == "GMT" {
		return time.FixedZone("UTC+00:00", 0), true
	}
	// Normalize optional UTC/GMT prefix
	v = strings.TrimPrefix(v, "UTC")
	v = strings.TrimPrefix(v, "GMT")
	v = strings.TrimSpace(v)
	// Patterns: +HH, -HH, +HHMM, -HHMM, +HH:MM, -HH:MM
	re := regexp.MustCompile(`^([+-])\s*(\d{1,2})(?::?(\d{2}))?$`)
	m := re.FindStringSubmatch(v)
	if m == nil {
		return nil, false
	}
	sign := 1
	if m[1] == "-" {
		sign = -1
	}
	hourStr := m[2]
	minStr := m[3]
	hours, err := strconv.Atoi(hourStr)
	if err != nil {
		return nil, false
	}
	mins := 0
	if minStr != "" {
		var err2 error
		mins, err2 = strconv.Atoi(minStr)
		if err2 != nil {
			return nil, false
		}
	}
	if hours < 0 || hours > 14 || mins < 0 || mins > 59 {
		return nil, false
	}
	offset := sign * ((hours * secondsInHour) + (mins * secondsInMinute))
	name := fmt.Sprintf("UTC%+03d:%02d", sign*hours, mins)
	return time.FixedZone(name, offset), true
}
