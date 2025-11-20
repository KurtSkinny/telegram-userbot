package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"telegram-userbot/internal/domain/filters"
	"telegram-userbot/internal/domain/notifications"
	"telegram-userbot/internal/infra/config"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/telegram/connection"
	"telegram-userbot/internal/infra/telegram/peersmgr"
	"telegram-userbot/internal/infra/telegram/status"
	versioninfo "telegram-userbot/internal/support/version"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"
)

// CommandExecutor - реализация интерфейса Executor
type CommandExecutor struct {
	client      *telegram.Client
	filters     *filters.FilterEngine
	notif       *notifications.Queue
	peers       *peersmgr.Service
	testRunning int64 // флаг выполнения команды test
}

// NewExecutor создает новый экземпляр CommandExecutor
func NewExecutor(
	client *telegram.Client,
	filterEngine *filters.FilterEngine,
	notif *notifications.Queue,
	peers *peersmgr.Service,
) *CommandExecutor {
	return &CommandExecutor{
		client:  client,
		filters: filterEngine,
		notif:   notif,
		peers:   peers,
	}
}

// Status возвращает текущий статус очереди уведомлений
func (e *CommandExecutor) Status(ctx context.Context) (*StatusResult, error) {
	if e.notif == nil {
		return nil, errors.New("queue is not available")
	}

	st := e.notif.Stats()
	return &StatusResult{
		UrgentQueueSize:    st.Urgent,
		RegularQueueSize:   st.Regular,
		LastRegularDrainAt: st.LastRegularDrainAt,
		LastFlushAt:        st.LastFlushAt,
		NextScheduleAt:     st.NextScheduleAt,
		Location:           st.Location,
	}, nil
}

// List возвращает список кешированных диалогов
func (e *CommandExecutor) List(ctx context.Context) (*ListResult, error) {
	if e.peers == nil {
		return nil, errors.New("peers manager is not available")
	}

	dialogs := e.peers.Dialogs()
	if len(dialogs) == 0 {
		return &ListResult{Dialogs: []Dialog{}}, nil
	}

	result := &ListResult{
		Dialogs: make([]Dialog, 0, len(dialogs)),
	}

	for _, item := range dialogs {
		dialog := e.buildDialog(ctx, item)
		result.Dialogs = append(result.Dialogs, dialog)
	}

	return result, nil
}

// buildDialog строит Dialog из DialogRef
func (e *CommandExecutor) buildDialog(ctx context.Context, ref peersmgr.DialogRef) Dialog {
	dialog := Dialog{
		ID:   ref.ID,
		Kind: string(ref.Kind),
	}

	if e.peers == nil {
		return dialog
	}

	resolved, ok, err := e.peers.ResolvePeer(ctx, ref.Kind, ref.ID)
	if err != nil || !ok {
		return dialog
	}

	switch v := resolved.(type) {
	case peers.User:
		raw := v.Raw()
		first := strings.TrimSpace(raw.FirstName)
		last := strings.TrimSpace(raw.LastName)
		fullName := strings.TrimSpace(strings.Join([]string{first, last}, " "))
		if fullName == "" {
			fullName = "<unknown>"
		}
		dialog.Title = fullName
		dialog.Username = strings.TrimPrefix(raw.Username, "@")
		if dialog.Username == "" {
			dialog.Username = "-"
		}

	case peers.Chat:
		raw := v.Raw()
		title := strings.TrimSpace(raw.Title)
		if title == "" {
			title = "<unknown chat>"
		}
		dialog.Title = title

	case peers.Channel:
		raw := v.Raw()
		title := strings.TrimSpace(raw.Title)
		if title == "" {
			title = "<untitled channel>"
		}
		dialog.Title = title
		dialog.Username = strings.TrimPrefix(raw.Username, "@")
		if dialog.Username == "" {
			dialog.Username = "-"
		}

		switch {
		case raw.Broadcast:
			dialog.Type = "Channel"
		case raw.Megagroup:
			dialog.Type = "Supergroup"
		default:
			dialog.Type = "Channel-like"
		}
	}

	return dialog
}

// Flush инициирует немедленный слив регулярной очереди уведомлений
func (e *CommandExecutor) Flush(ctx context.Context) error {
	if e.notif == nil {
		return errors.New("queue is not available")
	}

	e.notif.FlushImmediately("manual flush")
	return nil
}

// RefreshDialogs обновляет кеш диалогов из Telegram API
func (e *CommandExecutor) RefreshDialogs(ctx context.Context) error {
	if e.peers == nil {
		return errors.New("peers manager is not available")
	}

	if err := e.peers.RefreshDialogs(ctx, e.client.API()); err != nil {
		return fmt.Errorf("refresh dialogs failed: %w", err)
	}

	return nil
}

// ReloadFilters перезагружает фильтры и получателей из конфигурационных файлов
func (e *CommandExecutor) ReloadFilters(ctx context.Context) error {
	if e.filters == nil {
		return errors.New("filter engine is not available")
	}

	if err := e.filters.Load(); err != nil {
		return fmt.Errorf("reload filters failed: %w", err)
	}

	return nil
}

// Test отправляет тестовое сообщение администратору для проверки связности
func (e *CommandExecutor) Test(ctx context.Context) (*TestResult, error) {
	// Проверяем, не выполняется ли уже команда test
	if !atomic.CompareAndSwapInt64(&e.testRunning, 0, 1) {
		return nil, errors.New("test command is already running")
	}
	defer atomic.StoreInt64(&e.testRunning, 0)

	logger.Info("Test command invoked")

	if e.peers == nil {
		return nil, errors.New("peers manager is not available")
	}

	adminID := int64(config.Env().AdminUID)
	if adminID <= 0 {
		return nil, errors.New("admin UID is not configured")
	}

	currentTime := time.Now()
	message := fmt.Sprintf("Test message from userbot at %s", currentTime.Format(time.RFC3339))

	// Пытаемся отправить сообщение с повторными попытками
	const maxRetries = 3
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		connection.WaitOnline(ctx)
		status.GoOnline()

		peer, errPeer := e.peers.InputPeerByKind(ctx, filters.RecipientTypeUser.String(), adminID)
		if errPeer != nil {
			return nil, fmt.Errorf("resolve admin peer failed: %w", errPeer)
		}

		// Формируем получателя
		recipient := filters.Recipient{Type: filters.RecipientTypeUser, PeerID: filters.RecipientPeerID(adminID)}
		job := notifications.Job{
			ID:        time.Now().UnixNano(),
			CreatedAt: time.Now(),
		}
		randomID := notifications.RandomIDForMessage(job, recipient)

		// Отправка сообщения
		req := &tg.MessagesSendMessageRequest{
			Peer:     peer,
			Message:  message,
			RandomID: randomID,
		}

		_, apiErr := e.client.API().MessagesSendMessage(ctx, req)
		if apiErr == nil {
			logger.Infof("Test command: message sent successfully after %d attempt(s)", attempt)
			return &TestResult{
				Success: true,
				Message: fmt.Sprintf("Test message sent successfully to admin (id=%d)", adminID),
				SentAt:  currentTime,
			}, nil
		}

		lastErr = apiErr
		handled := connection.HandleError(apiErr)

		if handled && attempt < maxRetries {
			logger.Infof("Test command: network error occurred (attempt %d), retrying: %v", attempt, apiErr)
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return nil, errors.New("context cancelled during retry")
			}
			continue
		}
	}

	return nil, fmt.Errorf("all attempts failed: %w", lastErr)
}

// Whoami возвращает информацию о текущем аккаунте
func (e *CommandExecutor) Whoami(ctx context.Context) (*WhoamiResult, error) {
	if e.client == nil {
		return nil, errors.New("client is not available")
	}

	self, err := e.client.Self(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get self: %w", err)
	}

	fullname := strings.TrimSpace(strings.Join([]string{self.FirstName, self.LastName}, " "))
	if fullname == "" {
		fullname = "<unknown>"
	}

	return &WhoamiResult{
		ID:       self.ID,
		FullName: fullname,
		Username: self.Username,
	}, nil
}

// Version возвращает информацию о версии приложения
func (e *CommandExecutor) Version(ctx context.Context) (*VersionResult, error) {
	return &VersionResult{
		Name:    versioninfo.Name,
		Version: versioninfo.Version,
	}, nil
}
