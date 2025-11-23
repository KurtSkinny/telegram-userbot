package updates

import (
	"context"
	"fmt"
	"time"

	"telegram-userbot/internal/apptime"
	"telegram-userbot/internal/config"
	"telegram-userbot/internal/logger"

	"github.com/gotd/td/tg"
)

// handleAuthCommand обрабатывает команду auth от администратора
func (h *Handlers) handleAuthCommand(ctx context.Context, entities tg.Entities, msg *tg.Message) {
	_ = entities // Для совместимости с общей сигнатурой обработчиков команд
	logger.Info("Auth command received from admin")

	// Проверяем, включен ли веб-сервер
	if !config.Env().WebServerEnable {
		h.sendReply(ctx, msg, "❌ Web server is disabled. Enable it with WEB_SERVER_ENABLE=true in .env")
		return
	}

	// Проверяем, доступен ли webAuth
	if h.webAuth == nil {
		h.sendReply(ctx, msg, "❌ Web authentication service is not available")
		return
	}

	// Rate limiting: 1 токен в минуту
	h.authMu.Lock()
	timeSinceLastAuth := time.Since(h.lastAuthTime)
	if timeSinceLastAuth < time.Minute {
		h.authMu.Unlock()

		waitTime := time.Minute - timeSinceLastAuth
		message := fmt.Sprintf("⏳ Please wait %d seconds before requesting a new token.\n\n"+
			"Rate limit: 1 token per minute.", int(waitTime.Seconds()))
		h.sendReply(ctx, msg, message)

		logger.Debugf("Auth command rate limited, wait %v", waitTime)
		return
	}
	h.lastAuthTime = apptime.Now()
	h.authMu.Unlock()

	// Генерируем новый токен
	token := h.webAuth.GenerateAuthToken()

	// Формируем ссылку
	webAddr := config.Env().WebServerAddress
	authURL := fmt.Sprintf("http://%s/?token=%s", webAddr, token)

	// Отправляем ссылку администратору
	message := fmt.Sprintf("🔐 Web Interface Authentication\n\n"+
		"Click the link below to access the web interface:\n"+
		"%s\n\n"+
		"⚠️ Note:\n"+
		"• This link is valid for one-time use\n"+
		"• Session expires after 1 hour of inactivity\n"+
		"• Requesting a new auth will invalidate the previous session",
		authURL)

	h.sendReply(ctx, msg, message)
	logger.Info("Auth link sent to admin")
	if logger.IsDebugEnabled() {
		logger.Infof("Auth link: %s", authURL)
	}
}

// sendReply отправляет ответ на сообщение
func (h *Handlers) sendReply(ctx context.Context, msg *tg.Message, text string) {
	if h.api == nil {
		logger.Error("Cannot send reply: API client is nil")
		return
	}

	// Получаем InputPeer через peers manager
	var inputPeer tg.InputPeerClass
	if h.peers != nil {
		// Определяем тип и ID пира
		var peerKind string
		var peerID int64

		switch p := msg.PeerID.(type) {
		case *tg.PeerUser:
			peerKind = "user"
			peerID = p.UserID
		case *tg.PeerChat:
			peerKind = "chat"
			peerID = p.ChatID
		case *tg.PeerChannel:
			peerKind = "channel"
			peerID = p.ChannelID
		default:
			logger.Error("Unknown peer type")
			return
		}

		peer, err := h.peers.InputPeerByKind(ctx, peerKind, peerID)
		if err != nil {
			logger.Errorf("Failed to resolve peer: %v", err)
			return
		}
		inputPeer = peer
	} else {
		logger.Error("Peers manager is not available")
		return
	}

	// Генерируем RandomID для идемпотентности
	randomID := apptime.Now().UnixNano()

	_, err := h.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     inputPeer,
		Message:  text,
		RandomID: randomID,
		ReplyTo: &tg.InputReplyToMessage{
			ReplyToMsgID: msg.ID,
		},
	})

	if err != nil {
		logger.Errorf("Failed to send auth reply: %v", err)
	}
}
