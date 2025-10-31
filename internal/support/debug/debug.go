// Package debug — вспомогательные утилиты для отладки юзербота.
// Здесь сосредоточены функции печати входящих событий и тонкая обёртка над
// структурированным логированием. Цели:
//   - быстро просматривать апдейты в консоли (с нормальными именами авторов/чатов);
//   - писать структурные записи в общий лог только при активном DEBUG;
//   - минимизировать шум и резать слишком длинные тексты по границе рун.
// Пакет не влияет на бизнес‑логику и может быть выключен в проде переключателем DEBUG.

package debug

import (
	"context"
	"fmt"
	"strings"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/pr"
	"telegram-userbot/internal/infra/telegram/peersmgr"
	"unicode/utf8"

	tdpeers "github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
)

// DEBUG — глобальный переключатель режима отладки. Когда false, все функции пакета
// молчат. Переменная нечитаема из конфигурации автоматически: предполагается,
// что прод‑сборка запускается с DEBUG=false.
var DEBUG = true

const textPreviewLimit = 50

// PrintUpdate печатает компактное представление входящего сообщения в консоль.
// Формат: [prefix] <источник> > <читаемое имя>: <обрезанный текст>.
// Особенности:
//   - текст режется до безопасной длины по рунам, чтобы не ломать UTF‑8;
//   - для пользователей/чатов/каналов имена берутся из peersmgr.Service;
//   - entities передаются для потенциальных улучшений (сейчас не используются);
//   - отсутствующие метаданные заменяются плейсхолдерами ("<unknown>").
func PrintUpdate(prefix string, msg *tg.Message, entities tg.Entities, mgr *peersmgr.Service) {
	if !DEBUG {
		// Отладка выключена — ничего не делаем. Этот ранний выход упраздняет лишнюю работу.
		return
	}
	from, name := formatPeer(mgr, msg.PeerID)
	text := shortenMessage(msg.Message)
	pr.Printf("[%s] %s > %s: %s\n", prefix, from, name, text)
}

func shortenMessage(text string) string {
	if utf8.RuneCountInString(text) <= textPreviewLimit {
		return text
	}
	runes := []rune(text)
	return string(runes[:textPreviewLimit]) + "..."
}

func formatPeer(mgr *peersmgr.Service, peer tg.PeerClass) (string, string) {
	switch p := peer.(type) {
	case *tg.PeerUser:
		return userLabel(mgr, p.UserID)
	case *tg.PeerChat:
		return chatLabel(mgr, p.ChatID)
	case *tg.PeerChannel:
		return channelLabel(mgr, p.ChannelID)
	default:
		return "Unknown", fmt.Sprintf("%+v", peer)
	}
}

func userLabel(mgr *peersmgr.Service, id int64) (string, string) {
	raw := getUser(mgr, id)
	fullname := "<unknown>"
	if raw != nil {
		first := strings.TrimSpace(raw.FirstName)
		last := strings.TrimSpace(raw.LastName)
		if combined := strings.TrimSpace(strings.Join([]string{first, last}, " ")); combined != "" {
			fullname = combined
		}
	}
	return "User", fmt.Sprintf("'%s' (@%s) id: %d", fullname, usernameFromUser(raw), id)
}

func chatLabel(mgr *peersmgr.Service, id int64) (string, string) {
	title := "<unknown chat>"
	if raw := getChat(mgr, id); raw != nil {
		if trimmed := strings.TrimSpace(raw.Title); trimmed != "" {
			title = trimmed
		}
	}
	return "Chat", fmt.Sprintf("'%s' id: %d", title, id)
}

func channelLabel(mgr *peersmgr.Service, id int64) (string, string) {
	raw := getChannel(mgr, id)
	title := "<untitled channel>"
	if raw != nil {
		if trimmed := strings.TrimSpace(raw.Title); trimmed != "" {
			title = trimmed
		}
	}
	label := channelType(raw)
	return label, fmt.Sprintf("'%s' (@%s) id: %d", title, usernameFromChannel(raw), id)
}

func channelType(raw *tg.Channel) string {
	switch {
	case raw == nil:
		return "Channel-like"
	case raw.Broadcast:
		return "Channel"
	case raw.Megagroup:
		return "Supergroup"
	default:
		return "Channel-like"
	}
}

func getUser(mgr *peersmgr.Service, id int64) *tg.User {
	if mgr == nil {
		return nil
	}
	peer, ok, err := mgr.ResolvePeer(context.Background(), peersmgr.DialogKindUser, id)
	if err != nil || !ok {
		return nil
	}
	user, ok := peer.(tdpeers.User)
	if !ok {
		return nil
	}
	return user.Raw()
}

func getChat(mgr *peersmgr.Service, id int64) *tg.Chat {
	if mgr == nil {
		return nil
	}
	peer, ok, err := mgr.ResolvePeer(context.Background(), peersmgr.DialogKindChat, id)
	if err != nil || !ok {
		return nil
	}
	chat, ok := peer.(tdpeers.Chat)
	if !ok {
		return nil
	}
	return chat.Raw()
}

func getChannel(mgr *peersmgr.Service, id int64) *tg.Channel {
	if mgr == nil {
		return nil
	}
	peer, ok, err := mgr.ResolvePeer(context.Background(), peersmgr.DialogKindChannel, id)
	if err != nil || !ok {
		return nil
	}
	channel, ok := peer.(tdpeers.Channel)
	if !ok {
		return nil
	}
	return channel.Raw()
}

func usernameFromUser(raw *tg.User) string {
	if raw == nil {
		return "-"
	}
	if val := strings.TrimSpace(raw.Username); val != "" {
		if cleaned := strings.TrimPrefix(val, "@"); cleaned != "" {
			return cleaned
		}
	}
	return "-"
}

func usernameFromChannel(raw *tg.Channel) string {
	if raw == nil {
		return "-"
	}
	if val := strings.TrimSpace(raw.Username); val != "" {
		if cleaned := strings.TrimPrefix(val, "@"); cleaned != "" {
			return cleaned
		}
	}
	return "-"
}

// Debug пишет запись уровня Debug в общий лог только при активном DEBUG.
// Поля передаются как zap.Field для структурированного вывода.
func Debug(msg string, fields ...zap.Field) {
	if DEBUG {
		logger.Logger().Debug(msg, fields...)
	}
}

// Info пишет информационную запись при активном DEBUG. Поля — произвольные.
func Info(msg string, fields ...zap.Field) {
	if DEBUG {
		logger.Logger().Info(msg, fields...)
	}
}

// Warn пишет предупреждение в лог, если DEBUG=true.
func Warn(msg string, fields ...zap.Field) {
	if DEBUG {
		logger.Logger().Warn(msg, fields...)
	}
}

// Error пишет ошибку в лог при активном DEBUG, не паникует и не прерывает выполнение.
func Error(msg string, fields ...zap.Field) {
	if DEBUG {
		logger.Logger().Error(msg, fields...)
	}
}
