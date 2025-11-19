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
	"sync/atomic"
	"time"

	"telegram-userbot/internal/domain/filters"
	"telegram-userbot/internal/domain/notifications"
	"telegram-userbot/internal/infra/config"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/pr"
	"telegram-userbot/internal/infra/telegram/connection"
	"telegram-userbot/internal/infra/telegram/peersmgr"
	"telegram-userbot/internal/infra/telegram/status"
	versioninfo "telegram-userbot/internal/support/version"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/peers"
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
	client      *telegram.Client      // API-клиент Telegram (MTProto), нужен для команд теста/диагностики
	stopApp     context.CancelFunc    // внешняя отмена приложения (используется для команды exit и Ctrl-C на пустой строке)
	filters     *filters.FilterEngine // Движок фильтров: загрузка, хранение, матчи.
	notif       *notifications.Queue  // очередь уведомлений; нужна для flush/status
	peers       *peersmgr.Service     // peers-кэш, предоставляет офлайн-данные по диалогам
	cancel      context.CancelFunc    // локальная отмена run-цикла CLI
	wg          sync.WaitGroup        // ожидание завершения фоновой горутины run
	onceStart   sync.Once             // идемпотентный запуск
	onceStop    sync.Once             // идемпотентная остановка
	testRunning int64                 // флаг, указывающий, выполняется ли команда test в данный момент (0 - нет, 1 - да)
}

// NewService создаёт CLI-сервис. Параметр stopApp используется как «глобальная»
// остановка приложения (команда exit, Ctrl-C на пустой строке). Если notif задан,
// команда "flush" инициирует внеочередной слив регулярной очереди уведомлений.
func NewService(
	client *telegram.Client,
	stopApp context.CancelFunc,
	filterEngine *filters.FilterEngine,
	notif *notifications.Queue,
	peers *peersmgr.Service,
) *Service {
	return &Service{
		client:  client,
		stopApp: stopApp,
		filters: filterEngine,
		notif:   notif,
		peers:   peers,
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
		s.listDialogs(ctx)
	case "refresh dialogs":
		s.handleRefreshDialogs(ctx)
	case "reload":
		// Перезагружаем фильтры и получателей с rollback при ошибке
		if err := s.filters.Load(); err != nil {
			pr.ErrPrintln("reload filters error:", err)
		}
		pr.Println("recipients.json and filters.json reloaded")
	case "whoami":
		if res, err := whoAmI(ctx, s.client); err != nil {
			pr.ErrPrintln("whoami error:", err)
		} else {
			pr.Println(res)
		}
	case "test":
		s.handleTestAsync(ctx)
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

func (s *Service) handleRefreshDialogs(ctx context.Context) {
	if s.peers == nil {
		pr.ErrPrintln("peers manager is not available")
		return
	}

	if err := s.peers.RefreshDialogs(ctx, s.client.API()); err != nil {
		pr.ErrPrintln("refresh dialogs error:", err)
		return
	}
	pr.Println("Dialogs cache refreshed.")
}

// handleTestAsync запускает тестовую команду в отдельной горутине, если она не выполняется в данный момент.
func (s *Service) handleTestAsync(ctx context.Context) {
	// Проверяем, не выполняется ли уже команда test
	if !atomic.CompareAndSwapInt64(&s.testRunning, 0, 1) {
		pr.Println("Test command is already running, please wait...")
		return
	}

	// Запускаем выполнение в отдельной горутине
	go func() {
		// Гарантируем сброс флага при завершении
		defer atomic.StoreInt64(&s.testRunning, 0)

		// Создаем таймаут-контекст для выполнения команды
		const timeout = 100 * time.Second
		testCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		s.handleTest(testCtx)
	}()
}

// handleTest отправляет тестовое сообщение админу, чтобы проверить связность.
// Логика:
//  1. переводим статус в online (status.GoOnline),
//  2. резолвим adminID из конфигурации и получаем InputPeer через кэш,
//  3. ждём восстановления соединения (WaitOnline),
//  4. готовим детерминированный random_id для идемпотентности,
//  5. отправляем сообщение через MessagesSendMessage.
func (s *Service) handleTest(ctx context.Context) {
	logger.Info("CLI test command invoked")

	if s.peers == nil {
		logger.Error("peers manager is not available")
		return
	}

	adminID := int64(config.Env().AdminUID)
	if adminID <= 0 {
		logger.Error("CLI test command: admin UID is not configured")
	}

	currentTime := time.Now().Format(time.RFC3339)
	message := fmt.Sprintf("Test message from CLI at %s", currentTime)

	// Пытаемся отправить сообщение с повторными попытками при сетевых ошибках
	const maxRetries = 3
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		connection.WaitOnline(ctx)
		status.GoOnline()

		peer, errPeer := s.peers.InputPeerByKind(ctx, filters.RecipientTypeUser.String(), adminID)
		if errPeer != nil {
			logger.Errorf("CLI test command: resolve admin peer failed: %v", errPeer)
			return
		}

		// Формируем получателя в терминах доменной модели уведомлений.
		recipient := filters.Recipient{Type: filters.RecipientTypeUser, PeerID: filters.RecipientPeerID(adminID)}
		job := notifications.Job{
			ID:        time.Now().UnixNano(),
			CreatedAt: time.Now(),
		}
		randomID := notifications.RandomIDForMessage(job, recipient)

		// Готовим запрос отправки сообщения. Текст простой, без entities.
		req := &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  message,
			RandomID: randomID,
		}

		_, apiErr := s.client.API().MessagesSendMessage(ctx, req)
		if apiErr == nil {
			logger.Infof("CLI test command: message sent successfully after %d attempt(s)", attempt)
			lastErr = nil
			break
		} else {
			lastErr = apiErr
		}

		// Проверяем, является ли ошибка сетевой
		handled := connection.HandleError(apiErr)

		if handled {
			// Это сетевая ошибка, ожидаем восстановления соединения
			logger.Infof("CLI test command: network error occurred (attempt %d), waiting for connection: %v",
				attempt, apiErr)
			// connection.WaitOnline уже внутри вызова, но в случае сетевой ошибки
			// может потребоваться дополнительное ожидание
			if attempt < maxRetries {
				// Краткая пауза перед повторной попыткой
				select {
				case <-time.After(time.Second):
				case <-ctx.Done():
					logger.Info("CLI test command: context cancelled during retry delay")
					return
				}
				continue
			}
		}
	}

	if lastErr != nil {
		logger.Errorf("CLI test command: all attempts failed, final error: %v", lastErr)
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

// listDialogs выводит офлайн-снимок диалогов без сетевых запросов.
func (s *Service) listDialogs(ctx context.Context) {
	if s.peers == nil {
		pr.ErrPrintln("peers manager is not available")
		return
	}

	dialogs := s.peers.Dialogs()
	if len(dialogs) == 0 {
		pr.Println("No dialogs cached yet.")
		return
	}

	for _, item := range dialogs {
		s.printDialog(ctx, item)
	}
	pr.Printf("Total dialogs: %d\n", len(dialogs))
}

func (s *Service) printDialog(ctx context.Context, ref peersmgr.DialogRef) {
	var (
		rawUser    *tg.User
		rawChat    *tg.Chat
		rawChannel *tg.Channel
	)

	if s.peers != nil {
		if resolved, ok, err := s.peers.ResolvePeer(ctx, ref.Kind, ref.ID); err != nil {
			logger.Debugf("CLI list: resolve %s:%d failed: %v", ref.Kind, ref.ID, err)
		} else if ok {
			switch v := resolved.(type) {
			case peers.User:
				rawUser = v.Raw()
			case peers.Chat:
				rawChat = v.Raw()
			case peers.Channel:
				rawChannel = v.Raw()
			}
		}
	}

	switch ref.Kind {
	case peersmgr.DialogKindUser:
		s.printUser(ref.ID, rawUser)
	case peersmgr.DialogKindChat:
		s.printChat(ref.ID, rawChat)
	case peersmgr.DialogKindChannel:
		s.printChannel(ref.ID, rawChannel)
	case peersmgr.DialogKindFolder:
		pr.Printf("Folder: id: %d\n", ref.ID)
	default:
		pr.Printf("Unknown dialog kind %q id: %d\n", ref.Kind, ref.ID)
	}
}

func (s *Service) printUser(id int64, raw *tg.User) {
	if raw == nil {
		pr.Printf("User: id: %d (no cached metadata)\n", id)
		return
	}
	first := strings.TrimSpace(raw.FirstName)
	last := strings.TrimSpace(raw.LastName)
	fullName := strings.TrimSpace(strings.Join([]string{first, last}, " "))
	if fullName == "" {
		fullName = "<unknown>"
	}
	username := strings.TrimPrefix(raw.Username, "@")
	if username == "" {
		username = "-"
	}
	pr.Printf("User: '%s' (@%s) id: %d\n", fullName, username, id)
}

func (s *Service) printChat(id int64, raw *tg.Chat) {
	if raw == nil {
		pr.Printf("Chat: id: %d (no cached metadata)\n", id)
		return
	}
	title := strings.TrimSpace(raw.Title)
	if title == "" {
		title = "<unknown chat>"
	}
	pr.Printf("Chat: '%s' id: %d\n", title, id)
}

func (s *Service) printChannel(id int64, raw *tg.Channel) {
	if raw == nil {
		pr.Printf("Channel: id: %d (no cached metadata)\n", id)
		return
	}

	title := strings.TrimSpace(raw.Title)
	if title == "" {
		title = "<untitled channel>"
	}
	username := strings.TrimPrefix(raw.Username, "@")
	if username == "" {
		username = "-"
	}

	label := "Channel-like"
	if raw.Broadcast {
		label = "Channel"
	} else if raw.Megagroup {
		label = "Supergroup"
	}
	pr.Printf("%s: '%s' (@%s) id: %d\n", label, title, username, id)
}

// whoAmI возвращает строку с краткой информацией о текущем аккаунте (имя, username, id).
// Ошибку получения self оборачивает с контекстом.
func whoAmI(ctx context.Context, client *telegram.Client) (string, error) {
	self, err := client.Self(ctx)
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
