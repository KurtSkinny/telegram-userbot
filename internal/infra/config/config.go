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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"telegram-userbot/internal/infra/timeutil"

	"github.com/joho/godotenv"
)

// EnvConfig описывает параметры, приходящие из окружения (.env). Это «операционные»
// настройки запуска: учетные данные и файлы сессии для MTProto, лог-уровень,
// ограничения по скорости, флаги тестового DC, конфигурация уведомлений и т. д.
//
// NB: значения уже проходят минимальную валидацию и нормализацию в loadConfig.
// В рантайме по месту использования предполагается, что EnvConfig последователен.
type EnvConfig struct {
	APIID          int
	APIHash        string
	PhoneNumber    string
	SessionFile    string
	StateFile      string
	LogLevel       string
	ThrottleRPS    int
	DedupWindowSec int
	DebounceEditMS int
	TestDC         bool
	BotToken       string
	AdminUID       int
	AppTimezone    string
	FiltersFile    string
	RecipientsFile string
	PeersCacheFile string
	// Notification settings
	Notifier         string
	NotifyQueueFile  string
	NotifyFailedFile string
	NotifyTimezone   string
	NotifySchedule   []string
	// Notification cache settings
	NotifiedCacheFile string
	NotifiedTTLDays   int
	// Файловое логирование
	LogFile           string
	LogFileLevel      string
	LogFileMaxSize    int
	LogFileMaxBackups int
	LogFileMaxAge     int
	LogFileCompress   bool
	// Web Server
	WebServerEnable  bool
	WebServerAddress string
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
	defaultLogLevel          = "info"
	defaultSessionFile       = "data/session.bin"
	defaultStateFile         = "data/state.json"
	defaultNotifier          = "client"
	defaultNotifyQueueFile   = "data/notify_queue.json"
	defaultNotifyFailedFile  = "data/notify_failed.json"
	defaultNotifyTimezone    = "Europe/Moscow"
	defaultAppTimezone       = "Europe/Moscow"
	defaultNotifiedCacheFile = "data/notified_cache.json"
	defaultNotifiedTTLDays   = 30
	defaultFiltersFile       = "assets/filters.json"
	defaultRecipientsFile    = "assets/recipients.json"
	defaultPeersCacheFile    = "data/peers_cache.bbolt"
	// Файловое логирование (LOG_FILE не имеет дефолта - должен быть явно указан для активации)
	defaultLogFileLevel      = "debug"
	defaultLogFileMaxSize    = 50
	defaultLogFileMaxBackups = 3
	defaultLogFileMaxAge     = 7
	defaultLogFileCompress   = true
	// Web Server
	defaultWebServerEnable  = false
	defaultWebServerAddress = "127.0.0.1:8080"
)

var defaultNotifySchedule = []string{"08:00", "17:00"}

var (
	cfgInstance *Config
	cfgDone     bool
)

var AppLocation *time.Location

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
	logLevel := sanitizeLogLevel(os.Getenv("LOG_LEVEL"), defaultLogLevel, &warnings)
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
	logFile := strings.TrimSpace(os.Getenv("LOG_FILE"))
	logFileLevel := sanitizeLogLevel(os.Getenv("LOG_FILE_LEVEL"), defaultLogFileLevel, &warnings)
	logFileMaxSize := parseIntDefault("LOG_FILE_MAX_SIZE_MB", defaultLogFileMaxSize, greaterThanZero, &warnings)
	logFileMaxBackups := parseIntDefault("LOG_FILE_MAX_BACKUPS", defaultLogFileMaxBackups, nonNegative, &warnings)
	logFileMaxAge := parseIntDefault("LOG_FILE_MAX_AGE_DAYS", defaultLogFileMaxAge, nonNegative, &warnings)
	logFileCompress := parseBoolDefault("LOG_FILE_COMPRESS", defaultLogFileCompress, &warnings)
	// Web Server
	webServerEnable := parseBoolDefault("WEB_SERVER_ENABLE", defaultWebServerEnable, &warnings)
	webServerAddress := sanitizeFile("WEB_SERVER_ADDRESS", os.Getenv("WEB_SERVER_ADDRESS"),
		defaultWebServerAddress, &warnings)

	AppLocation, err = timeutil.ParseLocation(appTimezone)
	if err != nil {
		return nil, fmt.Errorf("invalid APP_TIMEZONE %q: %w", appTimezone, err)
	}

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
		AppTimezone:       appTimezone,
		NotifyTimezone:    notifyTimezone,
		NotifySchedule:    notifySchedule,
		NotifiedCacheFile: notifiedCacheFile,
		NotifiedTTLDays:   notifiedTTLDays,
		FiltersFile:       filtersFile,
		RecipientsFile:    recipientsFile,
		PeersCacheFile:    peersCacheFile,
		// Файловое логирование
		LogFile:           logFile,
		LogFileLevel:      logFileLevel,
		LogFileMaxSize:    logFileMaxSize,
		LogFileMaxBackups: logFileMaxBackups,
		LogFileMaxAge:     logFileMaxAge,
		LogFileCompress:   logFileCompress,
		// Web Server
		WebServerEnable:  webServerEnable,
		WebServerAddress: webServerAddress,
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

// parseBoolDefault читает name как bool. Если пусто/некорректно — возвращает defaultVal и пишет предупреждение.
func parseBoolDefault(name string, defaultVal bool, warnings *[]string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		appendWarningf(warnings, "env %s is not set; using default %v", name, defaultVal)
		return defaultVal
	}
	v, err := strconv.ParseBool(value)
	if err != nil {
		appendWarningf(warnings, "env %s value %q is not a valid boolean; using default %v", name, value, defaultVal)
		return defaultVal
	}
	return v
}

// sanitizeLogLevel нормализует LOG_LEVEL и ограничивает значения набором
// {debug, info, warn, error}. Всё остальное превращается в defaultLogLevel.
func sanitizeLogLevel(level string, defaultVal string, warnings *[]string) string {
	lvl := strings.ToLower(strings.TrimSpace(level))
	if lvl == "" {
		appendWarningf(warnings, "env LOG_LEVEL is not set; using default %q", defaultVal)
		return defaultVal
	}
	switch lvl {
	case "debug", "info", "warn", "error":
		return lvl
	default:
		appendWarningf(warnings, "env LOG_LEVEL value %q is invalid; using default %q", level, defaultVal)
		return defaultVal
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

// sanitizeTimezoneFlexible проверяет, что значение — корректная IANA‑зона или UTC‑смещение.
// При неудаче возвращает значение по умолчанию и добавляет предупреждение.
func sanitizeTimezoneFlexible(value string, fallback string, warnings *[]string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		appendWarningf(warnings, "env %s is not set; using default %q", "<timezone>", fallback)
		return fallback
	}
	if _, err := timeutil.ParseLocation(v); err != nil {
		appendWarningf(warnings, "timezone %q is invalid; using default %q", v, fallback)
		return fallback
	}
	return v
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
		if !timeutil.IsValidScheduleEntry(token) {
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

// cloneStrings создаёт копию среза строк. Используется, чтобы не делиться
// внутренними массивами и не ловить неожиданные мутации снаружи.
func cloneStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}
