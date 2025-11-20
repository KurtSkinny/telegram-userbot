// Package cli — интерактивная командная консоль для управления юзерботом.
// Сервис стартует фоном, читает команды из readline и взаимодействует с
// остальными подсистемами: клиентом Telegram (core.ClientCore), очередью
// уведомлений, кэшем Telegram и менеджером соединений. Поддерживается
// корректная интеграция в систему управления сервисами: Start/Stop идемпотентны.
package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"telegram-userbot/internal/domain/commands"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/pr"
)

// commandDescriptor описывает одну CLI-команду: её имя и краткое описание для help.
type commandDescriptor struct {
	name        string
	description string
}

// commandDescriptors — реестр доступных команд. Рендерится в help и подсказки.
// Важно: имена должны совпадать с кейсами в handleCommand().
var (
	commandDescriptors = []commandDescriptor{
		{name: "help", description: "Show available commands with short descriptions"},
		{name: "list", description: "Print cached dialogs (offline snapshot)"},
		{name: "refresh dialogs", description: "Fetch dialogs from API and update cache"},
		{name: "reload", description: "Reload filters.json and refresh filters"},
		{name: "status", description: "Show queue status (sizes, last drain, next schedule"},
		{name: "flush", description: "Drain regular queue immediately"},
		{name: "test", description: "Send current time to admin for connectivity check"},
		{name: "whoami", description: "Display information about the current account"},
		{name: "version", description: "Print userbot version"},
		{name: "exit", description: "Stop CLI and terminate the service"},
	}
)

// Service инкапсулирует CLI и интегрируется в систему управления сервисами приложения.
// Имеет собственный cancel, запускает цикл чтения команд в отдельной горутине
// и синхронно закрывается через Stop(). Потокобезопасность обеспечивается
// дисциплиной запуска/остановки и отсутствием внешних мутаций.
type Service struct {
	executor  commands.Executor  // Исполнитель команд
	stopApp   context.CancelFunc // внешняя отмена приложения (используется для команды exit и Ctrl-C на пустой строке)
	cancel    context.CancelFunc // локальная отмена run-цикла CLI
	wg        sync.WaitGroup     // ожидание завершения фоновой горутины run
	onceStart sync.Once          // идемпотентный запуск
	onceStop  sync.Once          // идемпотентная остановка
}

// NewService создаёт CLI-сервис. Параметр stopApp используется как «глобальная»
// остановка приложения (команда exit, Ctrl-C на пустой строке).
func NewService(
	executor commands.Executor,
	stopApp context.CancelFunc,
) *Service {
	return &Service{
		executor: executor,
		stopApp:  stopApp,
	}
}

// Start запускает основной цикл CLI в отдельной горутине. Повторные вызовы
// безопасно игнорируются. Контекст используется как родительский для run-цикла.
func (s *Service) Start(ctx context.Context) {
	s.onceStart.Do(func() {
		runCtx, cancel := context.WithCancel(ctx)
		s.cancel = cancel
		s.wg.Go(func() {
			s.run(runCtx)
		})
	})
}

// Stop завершает CLI: посылает внешнюю остановку приложения (если предусмотрено),
// прерывает readline, отменяет локальный контекст и дожидается завершения run-цикла.
func (s *Service) Stop() {
	s.onceStop.Do(func() {
		if s.stopApp != nil {
			s.stopApp()
		}
		if rl := pr.Rl(); rl != nil {
			pr.InterruptReadline()
		}
		if s.cancel != nil {
			s.cancel()
		}
		s.wg.Wait()
	})
}

// run — основной цикл обработчика CLI. Печатает подсказки, устанавливает обработчики
// клавиш и в цикле читает команды построчно, передавая их в handleCommand().
func (s *Service) run(ctx context.Context) {
	logger.Debug("CLI run started")
	pr.SetPrompt("> ")
	// Устанавливаем промпт и выводим краткую справку, чтобы пользователь не блуждал в темноте.
	pr.Println("CLI started. Enter commands:", joinCommandNames(commandDescriptors))
	pr.Println("Press '?' or type 'help' for detailed descriptions.")
	installKeyHandlers(s.stopApp)

	defer func() {
		if rl := pr.Rl(); rl != nil {
			_ = rl.Close()
		}
	}()

	// Главный цикл чтения команд. Выход — по отмене контекста или по EOF от readline.
	for {
		if ctx.Err() != nil {
			logger.Debug("CLI: context canceled")
			return
		}

		// Блокирующее чтение одной строки с учётом интерактивных обработчиков клавиш.
		line, err := pr.Rl().Readline()
		if err != nil {
			logger.Debug("CLI: deactivated (io.EOF)")
			return
		}

		cmd := strings.TrimSpace(line)
		if s.handleCommand(ctx, cmd) {
			logger.Debugf("CLI: command %q requested exit", cmd)
			return
		}
	}
}

// installKeyHandlers подключает обработчики специальных клавиш для readline:
//   - '?' — печать help без отправки символа в текущую строку;
//   - Ctrl-C на пустой строке — мягкая остановка приложения (stopApp) и прерывание readline;
//   - Ctrl-C на непустой строке — очистка текущей строки (как в типичных CLI).
func installKeyHandlers(stop context.CancelFunc) {
	rl := pr.Rl()
	if rl == nil || rl.Config == nil {
		return
	}

	// Сохраняем предыдущий listener, чтобы не ломать поведение по умолчанию.
	prev := rl.Config.Listener
	rl.Config.SetListener(func(line []rune, pos int, key rune) ([]rune, int, bool) {
		// Быстрая справка по командам по нажатию '?'.
		if key == '?' {
			printCommandHelp()
			if pos > 0 && pos <= len(line) {
				trimmed := append([]rune{}, line[:pos-1]...)
				trimmed = append(trimmed, line[pos:]...)
				return trimmed, pos - 1, true
			}
			return line, pos, true
		}
		// Ctrl-C (ETX): особое поведение — либо остановка приложения, либо очистка строки.
		if key == 3 { //nolint: mnd // Ctrl-C (ETX, rune value 3)
			trimmed := strings.TrimSpace(string(line))
			if trimmed == "" {
				if stop != nil {
					stop()
				}
				pr.InterruptReadline()
				return line, pos, true
			} else {
				// Clear the line if not empty (typical readline behavior)
				return []rune{}, 0, true
			}
		}
		if prev != nil {
			return prev.OnChange(line, pos, key)
		}
		return nil, 0, false
	})
}

// printCommandHelp печатает список поддерживаемых команд и их описания.
func printCommandHelp() {
	for _, text := range buildCommandHelpLines(commandDescriptors) {
		pr.Println(text)
	}
}

// handleCommand разбирает введённую команду и выполняет соответствующее действие.
// Возвращает true, если команда инициирует завершение CLI ("exit").
func (s *Service) handleCommand(ctx context.Context, cmd string) bool {
	switch cmd {
	case "help":
		printCommandHelp()
	case "list":
		pr.Println("Fetching dialogs...")
		s.handleList(ctx)
	case "refresh dialogs":
		s.handleRefreshDialogs(ctx)
	case "reload":
		s.handleReload(ctx)
	case "whoami":
		s.handleWhoami(ctx)
	case "test":
		s.handleTestAsync(ctx)
	case "version":
		s.handleVersion(ctx)
	case "flush":
		s.handleFlush(ctx)
	case "status":
		s.handleStatus(ctx)
	case "exit":
		if s.stopApp != nil {
			s.stopApp()
		}
		return true
	case "":
		// ignore
	default:
		pr.Println("unknown command:", cmd)
	}
	return false
}

func (s *Service) handleList(ctx context.Context) {
	result, err := s.executor.List(ctx)
	if err != nil {
		pr.ErrPrintln("list error:", err)
		return
	}

	if len(result.Dialogs) == 0 {
		pr.Println("No dialogs cached yet.")
		return
	}

	for _, d := range result.Dialogs {
		s.printDialog(d)
	}
	pr.Printf("Total dialogs: %d\n", len(result.Dialogs))
}

func (s *Service) handleRefreshDialogs(ctx context.Context) {
	if err := s.executor.RefreshDialogs(ctx); err != nil {
		pr.ErrPrintln("refresh dialogs error:", err)
		return
	}
	pr.Println("Dialogs cache refreshed.")
}

func (s *Service) handleReload(ctx context.Context) {
	if err := s.executor.ReloadFilters(ctx); err != nil {
		pr.ErrPrintln("reload filters error:", err)
		return
	}
	pr.Println("recipients.json and filters.json reloaded")
}

func (s *Service) handleWhoami(ctx context.Context) {
	result, err := s.executor.Whoami(ctx)
	if err != nil {
		pr.ErrPrintln("whoami error:", err)
		return
	}

	if result.Username != "" {
		pr.Println(fmt.Sprintf("You are: %s (@%s), id=%d", result.FullName, result.Username, result.ID))
	} else {
		pr.Println(fmt.Sprintf("You are: %s, id=%d", result.FullName, result.ID))
	}
}

func (s *Service) handleVersion(ctx context.Context) {
	result, err := s.executor.Version(ctx)
	if err != nil {
		pr.ErrPrintln("version error:", err)
		return
	}
	pr.ErrPrintln(fmt.Sprintf("%s v%s", result.Name, result.Version))
}

func (s *Service) handleFlush(ctx context.Context) {
	if err := s.executor.Flush(ctx); err != nil {
		pr.ErrPrintln("flush error:", err)
		return
	}
	pr.Println("Queue flush requested.")
}

// handleTestAsync запускает тестовую команду в отдельной горутине
func (s *Service) handleTestAsync(ctx context.Context) {
	// Запускаем выполнение в отдельной горутине
	go func() {
		// Создаем таймаут-контекст для выполнения команды
		const timeout = 100 * time.Second
		testCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		s.handleTest(testCtx)
	}()
}

// handleTest отправляет тестовое сообщение админу, чтобы проверить связность
func (s *Service) handleTest(ctx context.Context) {
	result, err := s.executor.Test(ctx)
	if err != nil {
		logger.Errorf("CLI test command failed: %v", err)
		return
	}

	if result.Success {
		logger.Info(result.Message)
	}
}

// handleStatus печатает агрегированное состояние очереди уведомлений
func (s *Service) handleStatus(ctx context.Context) {
	result, err := s.executor.Status(ctx)
	if err != nil {
		pr.ErrPrintln("status error:", err)
		return
	}

	pr.Printf("Queue status: urgent=%d regular=%d\n", result.UrgentQueueSize, result.RegularQueueSize)
	if !result.LastRegularDrainAt.IsZero() {
		pr.Printf("Last regular drain: %s\n",
			result.LastRegularDrainAt.In(result.Location).Format("2006-01-02 15:04:05"))
	} else {
		pr.Println("Last regular drain: <never>")
	}
	if !result.LastFlushAt.IsZero() {
		pr.Printf("Last persist: %s\n", result.LastFlushAt.In(result.Location).Format("2006-01-02 15:04:05"))
	} else {
		pr.Println("Last persist: <never>")
	}
	pr.Printf("Next schedule tick: %s\n", result.NextScheduleAt.In(result.Location).Format("2006-01-02 15:04:05"))
}

// printDialog печатает информацию о диалоге
func (s *Service) printDialog(d commands.Dialog) {
	switch d.Kind {
	case "user":
		pr.Printf("User: '%s' (@%s) id: %d\n", d.Title, d.Username, d.ID)
	case "chat":
		pr.Printf("Chat: '%s' id: %d\n", d.Title, d.ID)
	case "channel":
		label := d.Type
		if label == "" {
			label = "Channel-like"
		}
		pr.Printf("%s: '%s' (@%s) id: %d\n", label, d.Title, d.Username, d.ID)
	case "folder":
		pr.Printf("Folder: id: %d\n", d.ID)
	default:
		pr.Printf("Unknown dialog kind %q id: %d\n", d.Kind, d.ID)
	}
}

// joinCommandNames собирает строку имён команд, разделённых запятыми, для короткой подсказки.
func joinCommandNames(descriptors []commandDescriptor) string {
	names := make([]string, 0, len(descriptors))
	for _, d := range descriptors {
		names = append(names, d.name)
	}
	return strings.Join(names, ", ")
}

// buildCommandHelpLines генерирует строки помощи вида "<name> - <description>".
func buildCommandHelpLines(descriptors []commandDescriptor) []string {
	lines := make([]string, 0, len(descriptors)+1)
	lines = append(lines, "Available commands:")
	for _, descriptor := range descriptors {
		lines = append(lines, fmt.Sprintf("  %-8s - %s", descriptor.name, descriptor.description))
	}
	return lines
}
