// Пакет config отвечает за сбор и предоставление конфигурации всего приложения
// (userbot на MTProto). Он:
//  1. читает переменные окружения из .env (через godotenv),
//  2. загружает правила фильтрации из JSON-файла (filters.json),
//  3. нормализует и валидирует входные значения,
//  4. кеширует производные структуры (например, множество уникальных чатов),
//  5. предоставляет потокобезопасный доступ к результатам через R/W мьютекс.
//
// Бизнес-контекст: фильтры описывают, какие входящие сообщения нас интересуют
// (по словам, по регулярным выражениям и т. п.), из каких чатов их брать и куда
// слать уведомления (клиентом или ботом). Конфиг среды управляет подключением к
// Telegram API, скоростными лимитами, логированием, часовой зоной уведомлений и
// прочими «ручками».
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
)

// EnvConfig описывает параметры, приходящие из окружения (.env). Это «операционные»
// настройки запуска: учетные данные и файлы сессии для MTProto, лог-уровень,
// ограничения по скорости, флаги тестового DC, конфигурация уведомлений и т. д.
//
// NB: значения уже проходят минимальную валидацию и нормализацию в loadConfig.
// В рантайме по месту использования предполагается, что EnvConfig последователен.
type EnvConfig struct {
	APIID             int
	APIHash           string
	PhoneNumber       string
	SessionFile       string
	StateFile         string
	LogLevel          string
	ThrottleRPS       int
	DedupWindowSec    int
	DebounceEditMS    int
	TestDC            bool
	BotToken          string
	AdminUID          int
	Notifier          string
	NotifyQueueFile   string
	NotifyFailedFile  string
	NotifyTimezone    string
	AppTimezone       string
	NotifySchedule    []string
	NotifiedCacheFile string
	NotifiedTTLDays   int
	FiltersFile       string
	PeersCacheFile    string
	RecipientsFile    string  // НОВОЕ
}

// Config хранит конфигурацию среды.
//
// Потокобезопасность: публичные геттеры берут RLock. Перезагрузка фильтров
// (loadFilters) держит эксклюзивный Lock на время обновления полей.
type Config struct {
	Env      EnvConfig
	warnings []string     // предупреждения, накопленные при чтении окружения
	mu       sync.RWMutex // защита конкурентного доступа к конфигурации
}

// Значения по умолчанию для параметров окружения и связанных файлов.
const (
	defaultThrottleRPS       = 1
	defaultDedupWindowSec    = 120
	defaultDebounceEditMS    = 2000
	defaultAdminUID          = 0
	defaultLogLevel          = "debug"
	defaultSessionFile       = "data/session.bin"
	defaultStateFile         = "data/state.json"
	defaultNotifier          = "client"
	defaultNotifyQueueFile   = "data/notify_queue.json"
	defaultNotifyFailedFile  = "data/notify_failed.json"
	defaultNotifyTimezone    = "Europe/Moscow"
	defaultAppTimezone       = "UTC"
	defaultNotifiedCacheFile = "data/notified_cache.json"
	defaultNotifiedTTLDays   = 30
	defaultFiltersFile       = "assets/filters.json"
	defaultPeersCacheFile    = "data/peers_cache.bbolt"
)

var defaultNotifySchedule = []string{"08:00", "17:00"}

const defaultRecipientsFile = "assets/recipients.json"

var (
	cfgInstance *Config
	cfgDone     bool
)

// Load — точка входа для инициализации глобальной конфигурации всего приложения.
// При первом вызове:
//  1. читает .env,
//  2. формирует EnvConfig,
//  4. фиксирует результат в singleton cfgInstance.
//
// Повторный вызов запрещен (возвращается ошибка), чтобы избежать гонок
// конфигурации на старте.
func Load(envPath string) error {
	if cfgDone {
		return errors.New("config already loaded")
	}
	if cfgInstance == nil {
		cfgInstance = &Config{}
	}
	cfgInstance.mu.Lock()
	defer cfgInstance.mu.Unlock()
	newCfg, err := loadConfig(envPath)
	cfgInstance = newCfg
	cfgDone = true
	return err
}

// loadConfig выполняет фактическую загрузку/валидацию без установки глобального
// состояния. Удобно для тестов: можно собрать временный Config и проверить его.
func loadConfig(envPath string) (*Config, error) {
	if err := godotenv.Load(envPath); err != nil {
		return nil, fmt.Errorf("failed to load .env: %w", err)
	}

	apiID, err := parseRequiredInt("API_ID")
	if err != nil {
		return nil, err
	}

	apiHash := strings.TrimSpace(os.Getenv("API_HASH"))
	if apiHash == "" {
		return nil, errors.New("env API_HASH must be set")
	}

	phone := strings.TrimSpace(os.Getenv("PHONE_NUMBER"))
	if phone == "" {
		return nil, errors.New("env PHONE_NUMBER must be set")
	}

	var warnings []string

	throttleRPS := parseIntDefault("THROTTLE_RPS", defaultThrottleRPS, greaterThanZero, &warnings)
	dedupWindow := parseIntDefault("DEDUP_WINDOW_SEC", defaultDedupWindowSec, nonNegative, &warnings)
	debounceMS := parseIntDefault("DEBOUNCE_EDIT_MS", defaultDebounceEditMS, nonNegative, &warnings)
	adminUID := parseIntDefault("ADMIN_UID", defaultAdminUID, nonNegative, &warnings)
	logLevel := sanitizeLogLevel(os.Getenv("LOG_LEVEL"), &warnings)
	botToken := strings.TrimSpace(os.Getenv("BOT_TOKEN"))
	notifier := sanitizeNotifier(botToken, os.Getenv("NOTIFIER"), &warnings)
	sessionFile := sanitizeFile("SESSION_FILE", os.Getenv("SESSION_FILE"), defaultSessionFile, &warnings)
	stateFile := sanitizeFile("STATE_FILE", os.Getenv("STATE_FILE"), defaultStateFile, &warnings)
	testDC := strings.EqualFold(strings.TrimSpace(os.Getenv("TEST_DC")), "true")
	notifyQueueFile := sanitizeFile("NOTIFY_QUEUE_FILE", os.Getenv("NOTIFY_QUEUE_FILE"),
		defaultNotifyQueueFile, &warnings)
	notifyFailedFile := sanitizeFile("NOTIFY_FAILED_FILE", os.Getenv("NOTIFY_FAILED_FILE"),
		defaultNotifyFailedFile, &warnings)
	notifyTimezone := sanitizeTimezoneFlexible(os.Getenv("NOTIFY_TIMEZONE"), defaultNotifyTimezone, &warnings)
	appTimezone := sanitizeTimezoneFlexible(os.Getenv("APP_TIMEZONE"), defaultAppTimezone, &warnings)
	notifySchedule := sanitizeSchedule(os.Getenv("NOTIFY_SCHEDULE"), defaultNotifySchedule, &warnings)
	notifiedCacheFile := sanitizeFile("NOTIFIED_CACHE_FILE", os.Getenv("NOTIFIED_CACHE_FILE"),
		defaultNotifiedCacheFile, &warnings)
	notifiedTTLDays := parseIntDefault("NOTIFIED_CACHE_TTL_DAYS", defaultNotifiedTTLDays, greaterThanZero, &warnings)
	filtersFile := sanitizeFile("FILTERS_FILE", os.Getenv("FILTERS_FILE"), defaultFiltersFile, &warnings)
	peersCacheFile := sanitizeFile("PEERS_CACHE_FILE", os.Getenv("PEERS_CACHE_FILE"), defaultPeersCacheFile, &warnings)
	recipientsFile := sanitizeFile("RECIPIENTS_FILE", os.Getenv("RECIPIENTS_FILE"),
		defaultRecipientsFile, &warnings)

	env := EnvConfig{
		APIID:             apiID,
		APIHash:           apiHash,
		PhoneNumber:       phone,
		SessionFile:       sessionFile,
		StateFile:         stateFile,
		LogLevel:          logLevel,
		ThrottleRPS:       throttleRPS,
		DedupWindowSec:    dedupWindow,
		DebounceEditMS:    debounceMS,
		TestDC:            testDC,
		BotToken:          botToken,
		AdminUID:          adminUID,
		Notifier:          notifier,
		NotifyQueueFile:   notifyQueueFile,
		NotifyFailedFile:  notifyFailedFile,
		NotifyTimezone:    notifyTimezone,
		AppTimezone:       appTimezone,
		NotifySchedule:    notifySchedule,
		NotifiedCacheFile: notifiedCacheFile,
		NotifiedTTLDays:   notifiedTTLDays,
		FiltersFile:       filtersFile,
		PeersCacheFile:    peersCacheFile,
		RecipientsFile:    recipientsFile,  // НОВОЕ
	}

	cfg := &Config{
		Env:      env,
		warnings: warnings,
	}

	return cfg, nil
}

// Warnings возвращает накопленные предупреждения, возникшие при загрузке .env
// (например, когда подставлено значение по умолчанию). Возвращается копия.
func Warnings() []string {
	cfgInstance.mu.RLock()
	defer cfgInstance.mu.RUnlock()
	result := make([]string, len(cfgInstance.warnings))
	copy(result, cfgInstance.warnings)
	return result
}

// Env возвращает EnvConfig из глобального singleton. Это неизменяемый снимок
// на момент последней загрузки; для обновления надо перечитать конфиг целиком.
func Env() EnvConfig {
	return cfgInstance.Env
}

// parseRequiredInt читает обязательную целочисленную переменную окружения name.
// Если переменная не задана или не является корректным числом — возвращает ошибку.
// Используется для критичных параметров, без которых приложение не стартует.
func parseRequiredInt(name string) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return 0, fmt.Errorf("env %s must be set", name)
	}
	v, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("env %s must be a valid integer: %w", name, err)
	}
	return v, nil
}

// parseIntDefault читает name как int. Если пусто/некорректно/не проходит
// дополнительную проверку validator — возвращает defaultVal и пишет предупреждение.
// Это позволяет не падать на несущественных настройках и иметь дефолты.
func parseIntDefault(name string, defaultVal int, validator func(int) bool, warnings *[]string) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		appendWarningf(warnings, "env %s is not set; using default %d", name, defaultVal)
		return defaultVal
	}
	v, err := strconv.Atoi(value)
	if err != nil {
		appendWarningf(warnings, "env %s value %q is not a valid integer; using default %d", name, value, defaultVal)
		return defaultVal
	}
	if validator != nil && !validator(v) {
		appendWarningf(warnings, "env %s value %d does not satisfy constraints; using default %d", name, v, defaultVal)
		return defaultVal
	}
	return v
}

// appendWarningf — служебная функция для накопления предупреждений о некорректных
// переменных окружения. Список затем доступен через Warnings().
func appendWarningf(warnings *[]string, format string, args ...any) {
	if warnings == nil {
		return
	}
	*warnings = append(*warnings, fmt.Sprintf(format, args...))
}

// greaterThanZero/ nonNegative — простые валидаторы чисел. Используются в
// parseIntDefault, чтобы навязать смысловые ограничения без падения приложения.
func greaterThanZero(v int) bool { return v > 0 }
func nonNegative(v int) bool     { return v >= 0 }

// sanitizeLogLevel нормализует LOG_LEVEL и ограничивает значения набором
// {debug, info, warn, error}. Всё остальное превращается в defaultLogLevel.
func sanitizeLogLevel(level string, warnings *[]string) string {
	lvl := strings.ToLower(strings.TrimSpace(level))
	if lvl == "" {
		appendWarningf(warnings, "env LOG_LEVEL is not set; using default %q", defaultLogLevel)
		return defaultLogLevel
	}
	switch lvl {
	case "debug", "info", "warn", "error":
		return lvl
	default:
		appendWarningf(warnings, "env LOG_LEVEL value %q is invalid; using default %q", level, defaultLogLevel)
		return defaultLogLevel
	}
}

// sanitizeNotifier выбирает канал доставки уведомлений (client|bot). Если
// BOT_TOKEN пуст, принудительно используется client. Некорректные значения
// приводятся к defaultNotifier с записью предупреждения.
func sanitizeNotifier(botToken, notifier string, warnings *[]string) string {
	n := strings.ToLower(strings.TrimSpace(notifier))
	if n == "" {
		appendWarningf(warnings, "env NOTIFIER is not set; using default %q", defaultNotifier)
		return defaultNotifier
	}
	if strings.TrimSpace(botToken) == "" && n != "client" {
		appendWarningf(warnings, "env NOTIFIER forced to %q because BOT_TOKEN is empty", defaultNotifier)
		return defaultNotifier
	}
	if n == "client" || n == "bot" {
		return n
	}
	appendWarningf(warnings, "env NOTIFIER value %q is invalid; using default %q", notifier, defaultNotifier)
	return defaultNotifier
}

// sanitizeFile возвращает валидное имя файла конфигурации. Если переменная не
// задана, подставляет fallback и пишет предупреждение.
func sanitizeFile(name, value, fallback string, warnings *[]string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		appendWarningf(warnings, "env %s is not set; using default %q", name, fallback)
		return fallback
	}
	return v
}

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
	if loc, ok := parseUTCOffsetToLocation(v); ok {
		return loc, nil
	}
	return nil, fmt.Errorf("invalid timezone %q: not an IANA name or UTC offset", value)
}

// sanitizeTimezoneFlexible проверяет, что значение — корректная IANA‑зона или UTC‑смещение.
// При неудаче возвращает значение по умолчанию и добавляет предупреждение.
func sanitizeTimezoneFlexible(value string, fallback string, warnings *[]string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		appendWarningf(warnings, "env %s is not set; using default %q", "<timezone>", fallback)
		return fallback
	}
	if _, err := ParseLocation(v); err != nil {
		appendWarningf(warnings, "timezone %q is invalid; using default %q", v, fallback)
		return fallback
	}
	return v
}

// parseUTCOffsetToLocation парсит строки вида "+03:00", "-0700", "UTC+3", "GMT-04:30" или "Z".
// Возвращает фиксированную таймзону и ok=true при успешном разборе.
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
	offset := sign * ((hours * 60 * 60) + (mins * 60))
	name := fmt.Sprintf("UTC%+03d:%02d", sign*hours, mins)
	return time.FixedZone(name, offset), true
}

// sanitizeSchedule парсит CSV-строку формата "HH:MM,HH:MM,...", фильтрует
// некорректные записи, убирает дубликаты и возвращает итоговый список. При
// пустом результате подставляет fallback и пишет предупреждение.
func sanitizeSchedule(value string, fallback []string, warnings *[]string) []string {
	sort.Strings(fallback)
	raw := strings.TrimSpace(value)
	if raw == "" {
		appendWarningf(warnings, "env NOTIFY_SCHEDULE is not set; using default %v", fallback)
		return cloneStrings(fallback)
	}

	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		token := strings.TrimSpace(part)
		if token == "" {
			continue
		}
		if !isValidScheduleEntry(token) {
			appendWarningf(warnings, "env NOTIFY_SCHEDULE entry %q is invalid; expected HH:MM", token)
			continue
		}
		result = append(result, token)
	}

	if len(result) == 0 {
		appendWarningf(warnings, "env NOTIFY_SCHEDULE produced empty schedule; using default %v", fallback)
		return cloneStrings(fallback)
	}

	seen := make(map[string]struct{}, len(result))
	final := make([]string, 0, len(result))
	for _, token := range result {
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		final = append(final, token)
	}
	return final
}

// isValidScheduleEntry проверяет формат времени HH:MM и диапазоны часов/минут.
// Это простая синтаксическая проверка, логика исполнения расписания — снаружи.
func isValidScheduleEntry(value string) bool {
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

// cloneStrings создаёт копию среза строк. Используется, чтобы не делиться
// внутренними массивами и не ловить неожиданные мутации снаружи.
func cloneStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}
