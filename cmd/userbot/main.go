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
	"telegram-userbot/internal/infra/config"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/pr"
)

func main() {
	if err := pr.Init(); err != nil {
		logger.Fatal("failed to assigning stdout and stderr", zap.Error(err))
	}

	// envPath определяет расположение .env с секретами и общими настройками.
	envPath := flag.String("env", "assets/.env", "path to .env file")
	// // filtersPath указывает на JSON-файл с фильтрами, используемыми userbot.
	flag.Parse()

	// config.Load загружает конфигурацию из .env и других источников.
	if err := config.Load(*envPath); err != nil {
		logger.Fatal("failed to load config", zap.Error(err))
	}
	// Применяем часовую зону приложения (поддерживает IANA и UTC‑смещение). Влияет глобально на time.Local.
	if locApp, err := config.ParseLocation(config.Env().AppTimezone); err != nil {
		logger.Fatal("failed to parse APP_TIMEZONE", zap.Error(err))
	} else {
		time.Local = locApp //nolint:reassign // намеренно задаём часовую зону процесса (приложение работает в выбранной TZ)
	}

	// logger.Init задаёт уровень, а SetWriters перенаправляет выводы в подсистему pr (чтобы видеть логи в CLI UI).
	logger.Init(config.Env().LogLevel)
	logger.SetWriters(pr.Stdout(), pr.Stderr())
	for _, msg := range config.Warnings() {
		logger.Warn(msg)
	}

	// Контекст с обработкой системных сигналов (Ctrl+C/SIGTERM). Важно: stop() нужно вызвать, чтобы снять подписку.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	// Собираем приложение и передаём ему контекст жизненного цикла и stop как внешнюю CancelFunc.
	a := app.NewApp()
	if iniErr := a.Init(ctx, stop); iniErr != nil {
		stop()
		logger.Fatal("app init failed", zap.Error(iniErr))
	}

	// Запускаем основной цикл; блокируется до shutdown. Ошибки — фатальны, инициируем остановку и выходим.
	if runErr := a.Run(); runErr != nil {
		stop()
		logger.Fatal("app run failed", zap.Error(runErr))
	}
	// Освобождаем обработчик сигналов и закрываем ресурсы bootstrap-уровня.
	stop()
	logger.Info("Graceful shutdown complete")
}
