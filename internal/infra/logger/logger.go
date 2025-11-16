// Package logger — централизованная обёртка над zap для всего приложения.
// Позволяет инициализировать уровень логирования, форматирование, а также переназначать целевые потоки
// (stdout/stderr) на лету. Использует zap.AtomicLevel для динамической смены уровня и mutex для потокобезопасности.

package logger

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	// mu защищает доступ к глобальному состоянию логгера от одновременных изменений.
	mu sync.Mutex
	// log хранит текущий экземпляр zap.Logger, используемый во всём приложении.
	log *zap.Logger
	// logLevel управляет динамическим уровнем логирования без пересоздания ядра.
	logLevel = zap.NewAtomicLevelAt(zap.InfoLevel)
	// encoderCfg содержит настройки форматирования сообщений и обновляется при инициализации.
	encoderCfg = defaultEncoderConfig()
	// stdoutWriter определяет поток для стандартного вывода логов.
	stdoutWriter = zapcore.Lock(zapcore.AddSync(os.Stdout))
	// stderrWriter определяет поток для вывода ошибок логгера.
	stderrWriter = zapcore.Lock(zapcore.AddSync(os.Stderr))
)

// defaultEncoderConfig формирует консольный encoder с цветами и коротким caller.
// Формат времени фиксирован (YYYY-MM-DD HH:MM:SS). Для машинной обработки можно перейти на JSON-encoder.
func defaultEncoderConfig() zapcore.EncoderConfig {
	return zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalColorLevelEncoder,
		EncodeTime:     zapcore.TimeEncoderOfLayout("2006-01-02 15:04:05"),
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}
}

// rebuildLoggerLocked пересоздаёт глобальный логгер с текущими настройками потоков и уровнем.
// Предполагается, что вызывающий уже удерживает mu. AddCallerSkip(1) скрывает обёртки logger.*
// в стеке вызовов. Перед заменой предыдущий логгер аккуратно Sync(), чтобы сбросить буферы.
func rebuildLoggerLocked() {
	encoder := zapcore.NewConsoleEncoder(encoderCfg)
	core := zapcore.NewCore(encoder, stdoutWriter, logLevel)
	if log != nil {
		_ = log.Sync()
	}
	log = zap.New(core, zap.AddCaller(), zap.AddCallerSkip(1), zap.ErrorOutput(stderrWriter))
}

// Init инициализирует глобальный zap-логгер и настраивает уровень.
// Допустимые уровни: debug, info (по умолчанию), warn, error. Значение сравнивается без учёта регистра.
// Encoder берётся из defaultEncoderConfig. Потокобезопасно.
func Init(level string) {
	mu.Lock()
	defer mu.Unlock()

	switch strings.ToLower(level) {
	case "debug":
		logLevel.SetLevel(zap.DebugLevel)
	case "warn":
		logLevel.SetLevel(zap.WarnLevel)
	case "error":
		logLevel.SetLevel(zap.ErrorLevel)
	default:
		logLevel.SetLevel(zap.InfoLevel)
	}

	encoderCfg = defaultEncoderConfig()
	rebuildLoggerLocked()
}

// SetWriters переназначает целевые потоки логгера и пересобирает core.
// Можно вызывать в рантайме (например, чтобы писать в подсистему CLI). Nil означает Stdout/Stderr по умолчанию.
// Потокобезопасно.
func SetWriters(stdout, stderr io.Writer) {
	mu.Lock()
	defer mu.Unlock()

	if stdout == nil {
		stdoutWriter = zapcore.Lock(zapcore.AddSync(os.Stdout))
	} else {
		stdoutWriter = zapcore.Lock(zapcore.AddSync(stdout))
	}
	if stderr == nil {
		stderrWriter = zapcore.Lock(zapcore.AddSync(os.Stderr))
	} else {
		stderrWriter = zapcore.Lock(zapcore.AddSync(stderr))
	}

	rebuildLoggerLocked()
}

// Logger возвращает текущий zap.Logger, лениво создавая его при первом обращении.
// Возвращается "сырое" API (не Sugared); предпочтительнее передавать структурированные zap.Field.
func Logger() *zap.Logger {
	mu.Lock()
	defer mu.Unlock()

	if log == nil {
		rebuildLoggerLocked()
	}
	return log
}

// IsDebugEnabled проверяет, включен ли debug уровень логирования
// Добавьте эту функцию в ваш logger пакет, если её там нет
func IsDebugEnabled() bool {
	return Logger().Level() <= zap.DebugLevel
}

// Debug пишет структурированное сообщение уровня Debug.
func Debug(msg string, fields ...zap.Field) { Logger().Debug(msg, fields...) }

// Info пишет структурированное сообщение уровня Info.
func Info(msg string, fields ...zap.Field) { Logger().Info(msg, fields...) }

// Warn пишет структурированное предупреждение уровня Warn.
func Warn(msg string, fields ...zap.Field) { Logger().Warn(msg, fields...) }

// Error пишет структурированное сообщение об ошибке уровня Error.
func Error(msg string, fields ...zap.Field) { Logger().Error(msg, fields...) }

// Fatal пишет структурированное сообщение об ошибке уровня Fatal и завершает работу приложения.
func Fatal(msg string, fields ...zap.Field) {
	Logger().Fatal(msg, fields...)
	_ = Logger().Sync() // Обязательно сбросить буферы перед os.Exit
	os.Exit(1)
}

// Debugf форматирует сообщение через fmt.Sprintf. Используйте экономно:
// форматирование аллоцирует; для горячих путей предпочтительны структурированные поля.
func Debugf(msg string, a ...any) { Logger().Debug(fmt.Sprintf(msg, a...)) }

// Infof форматирует сообщение через fmt.Sprintf. Для горячих путей лучше использовать Info с полями.
func Infof(msg string, a ...any) { Logger().Info(fmt.Sprintf(msg, a...)) }

// Warnf форматирует сообщение через fmt.Sprintf. Предпочтительнее передавать данные через zap.Field.
func Warnf(msg string, a ...any) { Logger().Warn(fmt.Sprintf(msg, a...)) }

// Errorf форматирует сообщение через fmt.Sprintf. В критичных участках используйте Error с полями.
func Errorf(msg string, a ...any) { Logger().Error(fmt.Sprintf(msg, a...)) }
