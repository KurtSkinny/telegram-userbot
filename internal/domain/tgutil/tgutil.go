package tgutil

import "github.com/gotd/td/tg"

// GetPeerID нормализует получателя до его числового идентификатора (user/chat/channel).
// Возвращает 0 для неизвестного типа peer. Удобно для сопоставления фильтров по chat‑whitelist.
func GetPeerID(peer tg.PeerClass) int64 {
	switch p := peer.(type) {
	case *tg.PeerUser:
		return p.UserID
	case *tg.PeerChat:
		return p.ChatID
	case *tg.PeerChannel:
		return p.ChannelID
	default:
		return 0
	}
}
