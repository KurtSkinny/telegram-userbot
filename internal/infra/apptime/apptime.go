// Package apptime предоставляет централизованные функции для работы со временем в приложении.
// Все внутренние операции со временем должны использовать функции этого пакета для обеспечения
// единообразия работы с часовыми поясами согласно архитектурным принципам:
//
//  1. Все внутренние операции со временем выполняются в config.AppLocation
//  2. Уведомления учитывают индивидуальные часовые пояса получателей
//
// Этот пакет является единственной точкой входа для работы со временем в приложении.
package apptime

import (
	"time"

	"telegram-userbot/internal/infra/config"
	"telegram-userbot/internal/infra/timeutil"
)

// Now возвращает текущее время, сконвертированное в глобальную таймзону приложения (config.AppLocation).
// Эта функция должна использоваться для всех внутренних операций со временем:
// логирования, планирования событий, сохранения временных меток в базу данных и т.д.
func Now() time.Time {
	return time.Now().In(config.AppLocation)
}

// ToAppTime конвертирует любое время в глобальную таймзону приложения.
// Используется для нормализации входящих временных данных.
func ToAppTime(t time.Time) time.Time {
	return t.In(config.AppLocation)
}

// // ParseTimeInAppLocation парсит строковое представление времени и возвращает его в config.AppLocation.
// // Если время уже содержит информацию о часовом поясе, оно будет сконвертировано.
// // Если часовой пояс отсутствует, время будет интерпретировано как время в config.AppLocation.
// func ParseTimeInAppLocation(layout, value string) (time.Time, error) {
// 	t, err := time.Parse(layout, value)
// 	if err != nil {
// 		return time.Time{}, err
// 	}

// 	// Если время не содержит информацию о часовом поясе, интерпретируем его как время в AppLocation
// 	if t.Location() == time.UTC && !containsTimezone(layout) {
// 		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), config.AppLocation), nil
// 	}

// 	// Иначе конвертируем в AppLocation
// 	return t.In(config.AppLocation), nil
// }

// // FormatInAppLocation форматирует время в config.AppLocation согласно указанному layout.
// func FormatInAppLocation(t time.Time, layout string) string {
// 	return t.In(config.AppLocation).Format(layout)
// }

// FormatInTimezone форматирует время в указанной таймзоне.
// Если таймзона некорректна, использует config.AppLocation.
func FormatInTimezone(t time.Time, timezone, layout string) string {
	loc, err := timeutil.ParseLocation(timezone)
	if err != nil {
		loc = config.AppLocation
	}
	return t.In(loc).Format(layout)
}

// // ScheduleNextTime вычисляет следующее время срабатывания по расписанию в указанной таймзоне.
// func ScheduleNextTime(now time.Time, timezone string, schedule []string) time.Time {
// 	loc, err := timeutil.ParseLocation(timezone)
// 	if err != nil {
// 		loc = config.AppLocation
// 	}

// 	return nextScheduleAfter(now, loc, schedule)
// }

// // CalculateNotificationTime вычисляет время отправки уведомления с учетом:
// // 1. Срочности (срочные отправляются немедленно)
// // 2. Персонального часового пояса получателя
// // 3. Персонального расписания получателя
// // 4. Глобальных настроек как fallback
// func CalculateNotificationTime(
// 	urgent bool,
// 	now time.Time,
// 	recipientTZ string,
// 	recipientSchedule []string,
// 	fallbackTZ string,
// 	fallbackSchedule []string,
// ) time.Time {
// 	// Срочные уведомления отправляются немедленно в нормализованном времени приложения
// 	if urgent {
// 		return ToAppTime(now)
// 	}

// 	// Определяем эффективную таймзону получателя
// 	effectiveTZ := recipientTZ
// 	if effectiveTZ == "" {
// 		effectiveTZ = fallbackTZ
// 	}

// 	// Определяем эффективное расписание
// 	effectiveSchedule := recipientSchedule
// 	if len(effectiveSchedule) == 0 {
// 		effectiveSchedule = fallbackSchedule
// 	}

// 	return ScheduleNextTime(now, effectiveTZ, effectiveSchedule)
// }

// // containsTimezone проверяет, содержит ли layout информацию о часовом поясе
// func containsTimezone(layout string) bool {
// 	// Проверяем наличие основных timezone layout элементов
// 	return containsAny(layout, []string{"Z07", "Z0700", "Z07:00", "-0700", "-07:00", "MST", "Z070000", "Z07:00:00"})
// }

// // containsAny проверяет, содержит ли строка любую из подстрок
// func containsAny(s string, substrings []string) bool {
// 	for _, substr := range substrings {
// 		if len(s) >= len(substr) {
// 			for i := 0; i <= len(s)-len(substr); i++ {
// 				if s[i:i+len(substr)] == substr {
// 					return true
// 				}
// 			}
// 		}
// 	}
// 	return false
// }

// // nextScheduleAfter вычисляет следующий слот расписания после указанного времени.
// // Эта функция перенесена из recipients.go для централизации логики планирования.
// func nextScheduleAfter(now time.Time, location *time.Location, schedule []string) time.Time {
// 	localNow := now.In(location)
// 	today := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location)

// 	// Ищем ближайший слот сегодня
// 	for _, timeStr := range schedule {
// 		if !timeutil.IsValidScheduleEntry(timeStr) {
// 			continue
// 		}
// 		parts := splitTimeString(timeStr)
// 		if len(parts) != 2 {
// 			continue
// 		}

// 		hour, minute, valid := parseHourMinute(parts[0], parts[1])
// 		if !valid {
// 			continue
// 		}

// 		slot := time.Date(localNow.Year(), localNow.Month(), localNow.Day(),
// 			hour, minute, 0, 0, location)
// 		if slot.After(localNow) {
// 			return slot
// 		}
// 	}

// 	const hoursInDay = 24

// 	// Все слоты прошли - берем первый слот следующего дня
// 	if len(schedule) > 0 {
// 		timeStr := schedule[0]
// 		if timeutil.IsValidScheduleEntry(timeStr) {
// 			parts := splitTimeString(timeStr)
// 			if len(parts) == 2 {
// 				hour, minute, valid := parseHourMinute(parts[0], parts[1])
// 				if valid {
// 					nextDay := today.Add(hoursInDay * time.Hour)
// 					next := time.Date(nextDay.Year(), nextDay.Month(), nextDay.Day(),
// 						hour, minute, 0, 0, location)
// 					return next
// 				}
// 			}
// 		}
// 	}

// 	// Fallback - если расписание пустое, планируем на завтра в это же время
// 	return localNow.Add(hoursInDay * time.Hour)
// }

// // splitTimeString разделяет строку времени на части
// func splitTimeString(timeStr string) []string {
// 	result := make([]string, 0, 2)
// 	var current []rune

// 	for _, r := range timeStr {
// 		if r == ':' {
// 			if len(current) > 0 {
// 				result = append(result, string(current))
// 				current = current[:0]
// 			}
// 		} else {
// 			current = append(current, r)
// 		}
// 	}

// 	if len(current) > 0 {
// 		result = append(result, string(current))
// 	}

// 	return result
// }

// // parseHourMinute парсит час и минуту из строк
// func parseHourMinute(hourStr, minuteStr string) (hour, minute int, valid bool) {
// 	// Простой парсинг без strconv для избежания импорта
// 	hour, valid = parseSimpleInt(hourStr)
// 	if !valid || hour < 0 || hour > 23 {
// 		return 0, 0, false
// 	}

// 	minute, valid = parseSimpleInt(minuteStr)
// 	if !valid || minute < 0 || minute > 59 {
// 		return 0, 0, false
// 	}

// 	return hour, minute, true
// }

// // parseSimpleInt парсит простое неотрицательное число из строки
// func parseSimpleInt(s string) (int, bool) {
// 	if s == "" {
// 		return 0, false
// 	}

// 	result := 0
// 	for _, r := range s {
// 		if r < '0' || r > '9' {
// 			return 0, false
// 		}
// 		result = result*10 + int(r-'0')
// 		if result > 999999 { // защита от переполнения
// 			return 0, false
// 		}
// 	}

// 	return result, true
// }
