package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"telegram-userbot/internal/app"
	"telegram-userbot/internal/infra/concurrency"
	"telegram-userbot/internal/infra/config"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/pr"
	"telegram-userbot/internal/infra/timeutil"
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
	if err := config.Load(*envPath); err != nil {
		logger.Fatal("failed to load config", zap.Error(err))
	}
	// Применяем часовую зону приложения (поддерживает IANA и UTC‑смещение). Влияет глобально на time.Local.
	if locApp, err := timeutil.ParseLocation(config.Env().AppTimezone); err != nil {
		logger.Fatal("failed to parse APP_TIMEZONE", zap.Error(err))
	} else {
		time.Local = locApp //nolint:reassign // намеренно задаём часовую зону процесса (приложение работает в выбранной TZ)
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
