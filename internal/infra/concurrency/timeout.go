// Package concurrency — утилиты для безопасного конкурентного исполнения.
// В этом файле реализована функция автоматического таймаута приложения.
package concurrency

import (
	"context"
	"time"

	"go.uber.org/zap"

	"telegram-userbot/internal/infra/logger"
)

// StartTimeoutTimer запускает горутину с таймаутом, которая вызовет cancelFunc
// через указанное время. Это полезно для автоматического graceful shutdown
// приложения в тестовых сценариях или при ограниченном времени работы.
//
// Параметры:
//   - ctx: контекст для отслеживания отмены
//   - timeout: время через которое будет вызван cancel
//   - cancelFunc: функция отмены, которая будет вызвана по истечении таймаута
//
// Функция запускает отдельную горутину и завершается немедленно.
// Если контекст уже отменён или таймаут равен нулю, функция ничего не делает.
func StartTimeoutTimer(ctx context.Context, timeout int, cancelFunc context.CancelFunc) error {
	if timeout <= 0 || cancelFunc == nil {
		return nil
	}

	duration := time.Duration(timeout) * time.Second

	go func() {
		logger.Info("Auto-shutdown timer started", zap.Duration("timeout", duration))

		timer := time.NewTimer(duration)
		defer timer.Stop()

		select {
		case <-timer.C:
			logger.Info("Auto-shutdown timeout reached, initiating graceful shutdown")
			cancelFunc()
		case <-ctx.Done():
			// Контекст уже отменён, таймер больше не нужен
			logger.Debug("Auto-shutdown timer cancelled due to context cancellation")
			return
		}
	}()
	return nil
}
