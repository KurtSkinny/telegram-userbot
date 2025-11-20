// Пакет timeutil содержит служебные функции для работы со временем,
// например, парсинг таймзон или валидация формата времени.
package timeutil

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ParseLocation разбирает либо IANA‑таймзону (например, "Europe/Moscow"),
// либо UTC‑смещение (например, "+03:00", "-0700", "UTC+3", "GMT-04:30").
// Возвращает *time.Location или ошибку.
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
	if loc, ok := ParseUTCOffsetToLocation(v); ok {
		return loc, nil
	}
	return nil, fmt.Errorf("invalid timezone %q: not an IANA name or UTC offset", value)
}

// ParseUTCOffsetToLocation парсит строки вида "+03:00", "-0700", "UTC+3", "GMT-04:30" или "Z".
// Возвращает фиксированную таймзону и ok=true при успешном разборе.
func ParseUTCOffsetToLocation(value string) (*time.Location, bool) {
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
	const (
		secInHour = 60 * 60
		secInMin  = 60
	)
	offset := sign * ((hours * secInHour) + (mins * secInMin))
	name := fmt.Sprintf("UTC%+03d:%02d", sign*hours, mins)
	return time.FixedZone(name, offset), true
}

// IsValidScheduleEntry проверяет формат времени HH:MM и диапазоны часов/минут.
// Это простая синтаксическая проверка, логика исполнения расписания — снаружи.
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

// NormalizeLogTimestamp разбирает строковое представление времени в нескольких форматах
// и возвращает его в виде "2006-01-02 15:04:05" в указанной таймзоне.
// Если разбор не удался, возвращается исходная строка.
func NormalizeLogTimestamp(timeStr string, loc *time.Location) string {
	if timeStr == "" {
		return ""
	}
	var t time.Time
	var err error

	// Определяем возможные форматы
	layouts := []string{
		"2006-01-02T15:04:05.999-0700", // zap: millis + timezone без двоеточия
		"2006-01-02T15:04:05-0700",     // zap: без миллисекунд
		time.RFC3339,                   // ISO с ":" в таймзоне
		time.RFC3339Nano,               // nano
	}

	outputLayout := "2006-01-02 15:04:05"

	for _, layout := range layouts {
		if t, err = time.Parse(layout, timeStr); err == nil {
			break
		}
	}
	if err != nil {
		// Не удалось распарсить ни одним из форматов
		return timeStr
	}
	// Если таймзона не задана, используем UTC
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format(outputLayout)
}
