package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"

	"telegram-userbot/internal/app"
	"telegram-userbot/internal/concurrency"
	"telegram-userbot/internal/config"
	"telegram-userbot/internal/logger"
	"telegram-userbot/internal/pr"
)

func main() {
	if err := pr.Init(); err != nil {
		logger.Fatal("failed to assigning stdout and stderr", zap.Error(err))
	}

	// envPath определяет расположение .env с секретами и общими настройками.
	envPath := flag.String("env", "assets/.env", "path to .env file")
	envTimeout := flag.Int("timeout", 0, "application timeout in seconds (0 means no timeout)")

	// // filtersPath указывает на JSON-файл с фильтрами, используемыми userbot.
	flag.Parse()

	// config.Load загружает конфигурацию из .env и других источников.
	if _, err := config.LoadOnce(*envPath); err != nil {
		logger.Fatal("failed to load config", zap.Error(err))
	}

	// logger.Init задаёт уровень, а SetWriters перенаправляет выводы в подсистему pr (чтобы видеть логи в CLI UI).
	logger.Init()
	logger.SetWriters(pr.Stdout(), pr.Stderr())
	for _, msg := range config.Warnings() {
		logger.Warn(msg)
	}

	// Создаём контекст приложения, который будет отменён при получении сигнала завершения.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Запускаем таймер авто-выключения, если указан таймаут.
	if err := concurrency.StartTimeoutTimer(ctx, *envTimeout, cancel); err != nil {
		logger.Fatal("failed to start timeout timer", zap.Error(err))
	}

	// Создаём и запускаем приложение.
	a := app.NewApp(ctx, cancel)
	if err := a.Run(); err != nil {
		cancel()
		logger.Fatal("app init failed", zap.Error(err))
	}

	logger.Info("Shutdown complete")
}
