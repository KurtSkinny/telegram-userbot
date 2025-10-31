// Package main — точка входа CLI для userbot.
// Здесь парсим флаги, загружаем конфигурацию, настраиваем логирование и
// организуем корректное завершение по системным сигналам (Ctrl+C/SIGTERM).
// Главная задача: инициализировать App и отдать ему управление, обеспечив graceful shutdown.

package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"telegram-userbot/internal/app"
	"telegram-userbot/internal/infra/config"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/pr"
)

// main поднимает окружение, стартует приложение и блокируется до завершения.
// Порядок:
//  1. bootstrap: stdout/stderr → pr, базовый log с префиксом времени,
//  2. flags/env: пути к .env и filters.json,
//  3. config: загрузка и предупреждения,
//  4. logger: уровень и перенаправление вывода в pr,
//  5. signals: контекст с отменой по Ctrl+C/SIGTERM (stop обязателен к вызову),
//  6. app: Init(ctx, stop) и Run().
func main() {
	log.SetFlags(0)
	log.SetPrefix(time.Now().Format("2006-01-02 15:04:05 "))
	// Префикс времени на уровне bootstrap до инициализации внутреннего logger; далее пишем через logger.
	if err := pr.Init(); err != nil {
		log.Fatalf("failed to assigning stdout and stderr: %v", err)
	}

	// envPath определяет расположение .env с секретами и общими настройками.
	envPath := flag.String("env", "assets/.env", "path to .env file")
	// // filtersPath указывает на JSON-файл с фильтрами, используемыми userbot.
	flag.Parse()

	// config.Load загружает конфигурацию из .env и других источников.
	if err := config.Load(*envPath); err != nil {
		log.Fatalf("failed to load config: %v", err)
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
		log.Fatalf("app init failed: %v", iniErr)
	}

	// Запускаем основной цикл; блокируется до shutdown. Ошибки — фатальны, инициируем остановку и выходим.
	if runErr := a.Run(); runErr != nil {
		stop()
		log.Fatalf("app run failed: %v", runErr)
	}
	// Освобождаем обработчик сигналов и закрываем ресурсы bootstrap-уровня.
	stop()
	log.Println("Graceful shutdown complete")
}
