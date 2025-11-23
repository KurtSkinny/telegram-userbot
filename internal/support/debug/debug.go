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
	"telegram-userbot/internal/logger"
	"telegram-userbot/internal/pr"
	"telegram-userbot/internal/telegram/peersmgr"
	"unicode/utf8"

	tdpeers "github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"
)

const textPreviewLimit = 50

// PrintUpdate печатает в лог отладочную информацию об апдейте сообщения msg.
func PrintUpdate(prefix string, msg *tg.Message, entities tg.Entities, mgr *peersmgr.Service) {
	if !logger.IsDebugEnabled() {
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
