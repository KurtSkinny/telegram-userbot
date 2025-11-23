package web

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"telegram-userbot/internal/logger"

	"go.uber.org/zap"
)

// writeResponse записывает ответ в ResponseWriter с автоматическим логированием ошибок.
// Автоматически определяет место вызова для отладки.
func writeResponse(w http.ResponseWriter, data []byte) {
	var writeErr error

	if _, writeErr = w.Write(data); writeErr == nil {
		return
	}

	// Получаем информацию о вызывающей функции
	callerLocation := "unknown"
	if _, file, line, ok := runtime.Caller(1); ok {
		if wd, getwdErr := os.Getwd(); getwdErr == nil {
			if rel, relErr := filepath.Rel(wd, file); relErr == nil {
				file = rel
			}
		}
		callerLocation = file + ":" + strconv.Itoa(line)
	}

	logger.Error("failed to write response",
		zap.String("caller", callerLocation),
		zap.Error(writeErr))
}
