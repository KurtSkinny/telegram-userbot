// Package cli — интерактивная командная консоль для управления юзерботом.
// Сервис стартует фоном, читает команды из readline и взаимодействует с
// остальными подсистемами: клиентом Telegram (core.ClientCore), очередью
// уведомлений, кэшем Telegram и менеджером соединений. Поддерживается
// корректная интеграция в lifecycle: Start/Stop идемпотентны.
package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"telegram-userbot/internal/adapters/telegram/core"
	"telegram-userbot/internal/domain/filters"
	"telegram-userbot/internal/domain/notifications"
	"telegram-userbot/internal/infra/config"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/pr"
	"telegram-userbot/internal/infra/telegram/cache"
	"telegram-userbot/internal/infra/telegram/connection"
	"telegram-userbot/internal/infra/telegram/status"
	versioninfo "telegram-userbot/internal/support/version"

	"github.com/gotd/td/tg"
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
		{name: "list", description: "Print cached dialogs"},
		{name: "reload", description: "Reload filters.json and refresh filters"},
		{name: "status", description: "Show queue status (sizes, last drain, next schedule"},
		{name: "flush", description: "Drain regular queue immediately"},
		{name: "test", description: "Send current time to admin for connectivity check"},
		{name: "whoami", description: "Display information about the current account"},
		{name: "version", description: "Print userbot version"},
		{name: "exit", description: "Stop CLI and terminate the service"},
	}
)

// Service инкапсулирует CLI и интегрируется в lifecycle приложения.
// Имеет собственный cancel, запускает цикл чтения команд в отдельной горутине
// и синхронно закрывается через Stop(). Потокобезопасность обеспечивается
// дисциплиной запуска/остановки и отсутствием внешних мутаций.
type Service struct {
	cl        *core.ClientCore      // API-клиент Telegram (MTProto), нужен для команд теста/диагностики
	stopApp   context.CancelFunc    // внешняя отмена приложения (используется для команды exit и Ctrl-C на пустой строке)
	filters   *filters.FilterEngine // Движок фильтров: загрузка, хранение, матчи.
	notif     *notifications.Queue  // очередь уведомлений; нужна для flush/status
	cancel    context.CancelFunc    // локальная отмена run-цикла CLI
	wg        sync.WaitGroup        // ожидание завершения фоновой горутины run
	onceStart sync.Once             // идемпотентный запуск
	onceStop  sync.Once             // идемпотентная остановка
}

// NewService создаёт CLI-сервис. Параметр stopApp используется как «глобальная»
// остановка приложения (команда exit, Ctrl-C на пустой строке). Если notif задан,
// команда "flush" инициирует внеочередной слив регулярной очереди уведомлений.
func NewService(
	cl *core.ClientCore,
	stopApp context.CancelFunc,
	filterEngine *filters.FilterEngine,
	notif *notifications.Queue,
) *Service {
	return &Service{cl: cl, stopApp: stopApp, filters: filterEngine, notif: notif}
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
		if s.handleCommand(cmd) {
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
func (s *Service) handleCommand(cmd string) bool {
	switch cmd {
	case "help":
		printCommandHelp()
	case "list":
		pr.Println("Fetching dialogs...")
		listDialogs()
	case "reload":
		if err := s.filters.GetFilters(); err != nil {
			pr.ErrPrintln("reload error:", err)
		} else {
			pr.Println("filters.json reloaded")
		}
	case "whoami":
		if res, err := whoAmI(s.cl); err != nil {
			pr.ErrPrintln("whoami error:", err)
		} else {
			pr.Println(res)
		}
	case "test":
		s.handleTest()
	case "version":
		pr.ErrPrintln(fmt.Sprintf("%s v%s", versioninfo.Name, versioninfo.Version))
	case "flush":
		// Внеплановый слив регулярной очереди.
		if s.notif != nil {
			s.notif.FlushImmediately("cli flush")
			pr.Println("Queue flush requested.")
		} else {
			pr.ErrPrintln("queue is not available")
		}
	case "status":
		s.handleStatus()
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

// handleTest отправляет тестовое сообщение админу, чтобы проверить связность.
// Логика:
//  1. переводим статус в online (status.GoOnline),
//  2. резолвим adminID из конфигурации и получаем InputPeer через кэш,
//  3. ждём восстановления соединения (WaitOnline),
//  4. готовим детерминированный random_id для идемпотентности,
//  5. отправляем сообщение через MessagesSendMessage.
func (s *Service) handleTest() {
	logger.Info("CLI test command invoked")

	// const tens = 10
	// ctx, cancel := context.WithTimeout(context.Background(), tens*time.Second)
	// defer cancel()

	// adminID := int64(config.Env().AdminUID)
	// peer, err := cache.GetInputPeerUser(adminID)
	// if err != nil {
	// 	logger.Errorf("CLI test command: resolve admin peer failed: %v", err)
	// }
	// res, err := s.cl.API.MessagesSetTyping(ctx, &tg.MessagesSetTypingRequest{
	// 	Peer:   peer,
	// 	Action: &tg.SendMessageTypingAction{},
	// })
	// connection.HandleError(err)
	// pr.Printf("CLI test command resul: res=%#v, err=%#v\n", res, err)

	status.GoOnline()

	adminID := int64(config.Env().AdminUID)
	if adminID <= 0 {
		logger.Error("CLI test command: admin UID is not configured")
	}

	currentTime := time.Now().Format(time.RFC3339)
	message := fmt.Sprintf("Test message from CLI at %s", currentTime)
	logger.Infof("CLI test command: preparing message for admin %d", adminID)

	peer, err := cache.GetInputPeerUser(adminID)
	if err != nil {
		logger.Errorf("CLI test command: resolve admin peer failed: %v", err)
	}

	const ten = 10
	ctx, cancel := context.WithTimeout(context.Background(), ten*time.Second)
	defer cancel()

	logger.Info("CLI test command: waiting for connection readiness")
	connection.WaitOnline(ctx)

	// Формируем получателя в терминах доменной модели уведомлений.
	recipient := notifications.Recipient{Type: notifications.RecipientTypeUser, ID: adminID}
	// Простейший уникальный ID «задания» — текущее время в наносекундах. Достаточно для CLI.
	jobID := time.Now().UnixNano() // используем текущее время как уникальный идентификатор "задания"
	const firstRecipientIndex = 0  // индекс получателя в рамках задания (избегаем magic number)
	randomID := notifications.RandomIDForMessage(jobID, recipient, firstRecipientIndex)

	// Готовим запрос отправки сообщения. Текст простой, без entities.
	req := &tg.MessagesSendMessageRequest{
		Peer:     peer,
		Message:  message,
		RandomID: randomID,
	}

	logger.Infof("CLI test command: sending message \"%s\"", message)

	// if err := s.notif.Send(ctx, adminID, message); err != nil {
	connection.WaitOnline(ctx)
	_, err = s.cl.API.MessagesSendMessage(ctx, req)
	if err != nil {
		handled := connection.HandleError(err)
		logger.Errorf("CLI test command: send failed (handled=%t): %v", handled, err)
	}

	logger.Info("CLI test command: complited")
}

// handleStatus печатает агрегированное состояние очереди уведомлений: размеры, метки времени
// последнего дренирования и флаша, а также следующего планового тика. Временные метки
// приводятся к локальной таймзоне, заданной в статистике очереди.
func (s *Service) handleStatus() {
	if s.notif == nil {
		pr.ErrPrintln("queue is not available")
		return
	}
	st := s.notif.Stats()
	pr.Printf("Queue status: urgent=%d regular=%d\n", st.Urgent, st.Regular)
	if !st.LastRegularDrainAt.IsZero() {
		pr.Printf("Last regular drain: %s\n", st.LastRegularDrainAt.In(st.Location).Format(time.RFC3339))
	} else {
		pr.Println("Last regular drain: <never>")
	}
	if !st.LastFlushAt.IsZero() {
		pr.Printf("Last persist: %s\n", st.LastFlushAt.In(st.Location).Format(time.RFC3339))
	} else {
		pr.Println("Last persist: <never>")
	}
	pr.Printf("Next schedule tick: %s\n", st.NextScheduleAt.In(st.Location).Format(time.RFC3339))
}

// listDialogs выводит кэшированные диалоги (из cache.Dialogs) в человекочитаемом виде.
// Функция не выполняет сетевых запросов и работает только по локальному кэшу.
func listDialogs() {
	dialogs := cache.Dialogs()
	if len(dialogs) == 0 {
		pr.Println("No dialogs cached yet.")
		return
	}
	for _, d := range dialogs {
		printDialogCached(d)
	}
	pr.Printf("Total dialogs: %d\n", len(dialogs))
}

// printDialogCached печатает одну запись диалога по данным из кэша, корректно обрабатывая
// пользователей, чаты, каналы и папки. Для неизвестных типов выводит отладочную дампу.
func printDialogCached(d tg.DialogClass) {
	switch dlg := d.(type) {
	case *tg.Dialog:
		switch peer := dlg.Peer.(type) {
		case *tg.PeerUser:
			first, _ := cache.UserFirstName(peer.UserID)
			last, _ := cache.UserLastName(peer.UserID)
			username, _ := cache.UserUsername(peer.UserID)
			fullname := strings.TrimSpace(first + " " + last)
			if fullname == "" {
				fullname = "<unknown>"
			}
			pr.Printf("User: '%s' (@%s) id: %d\n", fullname, username, peer.UserID)
		case *tg.PeerChat:
			title, _ := cache.ChatTitle(peer.ChatID)
			if title == "" {
				title = "<unknown chat>"
			}
			pr.Printf("Chat: '%s' id: %d\n", title, peer.ChatID)
		case *tg.PeerChannel:
			title, _ := cache.ChannelTitle(peer.ChannelID)
			username, _ := cache.ChannelUsername(peer.ChannelID)
			broadcast, megagroup, _ := cache.ChannelFlags(peer.ChannelID)
			label := "Channel-like"
			if broadcast {
				label = "Channel"
			} else if megagroup {
				label = "Supergroup"
			}
			if title == "" {
				title = "<untitled channel>"
			}
			pr.Printf("%s: '%s' (@%s) id: %d\n", label, title, username, peer.ChannelID)
		default:
			pr.Printf("Unknown peer: %+v\n", peer)
		}
	case *tg.DialogFolder:
		folder := dlg.Folder
		title := strings.TrimSpace(folder.Title)
		if title == "" {
			title = "<unnamed folder>"
		}
		pr.Printf("Folder: '%s' id: %d\n", title, folder.ID)
	default:
		pr.Printf("Unknown dialog: %+v\n", dlg)
	}
}

// whoAmI возвращает строку с краткой информацией о текущем аккаунте (имя, username, id).
// Ошибку получения self оборачивает с контекстом.
func whoAmI(cl *core.ClientCore) (string, error) {
	self, err := cl.Client.Self(context.Background())
	if err != nil {
		return "", fmt.Errorf("failed to get self: %w", err)
	}
	fullname := strings.TrimSpace(strings.Join([]string{self.FirstName, self.LastName}, " "))
	if fullname == "" {
		fullname = "<unknown>"
	}
	if self.Username != "" {
		return fmt.Sprintf("You are: %s (@%s), id=%d", fullname, self.Username, self.ID), nil
	}
	return fmt.Sprintf("You are: %s, id=%d", fullname, self.ID), nil
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
