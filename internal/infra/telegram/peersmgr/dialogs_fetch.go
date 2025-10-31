package peersmgr

import (
	"context"
	"errors"
	"fmt"

	tgruntime "telegram-userbot/internal/infra/telegram/runtime"

	"github.com/gotd/td/tg"
)

const (
	dialogFetchWaitMinMs  = 500
	dialogFetchWaitMaxMs  = 1500
	dialogFetchPageLimit  = 100
	dialogFetchZeroOffset = 0
)

var errDialogsNotModified = errors.New("dialogs not modified")

// fetchDialogs последовательно выгружает весь список диалогов пользователя через MessagesGetDialogs.
// Реализована пагинация по (offset_date, offset_id, offset_peer) с использованием заранее собранных access_hash.
func fetchDialogs(ctx context.Context, api *tg.Client) (*tg.MessagesDialogs, error) {
	result := &tg.MessagesDialogs{}

	offsetDate := dialogFetchZeroOffset
	offsetID := dialogFetchZeroOffset
	var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}

	userHashes := make(map[int64]int64)
	channelHashes := make(map[int64]int64)

	tgruntime.WaitRandomTimeMs(ctx, dialogFetchWaitMinMs, dialogFetchWaitMaxMs)

	for {
		resp, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate,
			OffsetID:   offsetID,
			OffsetPeer: offsetPeer,
			Limit:      dialogFetchPageLimit,
		})
		if err != nil {
			return nil, fmt.Errorf("MessagesGetDialogs: %w", err)
		}

		batch, err := normalizeDialogsResponse(resp)
		if err != nil {
			if errors.Is(err, errDialogsNotModified) {
				return result, nil
			}
			return nil, err
		}

		if len(batch.Dialogs) == 0 {
			break
		}

		result.Dialogs = append(result.Dialogs, batch.Dialogs...)
		result.Messages = append(result.Messages, batch.Messages...)
		result.Chats = append(result.Chats, batch.Chats...)
		result.Users = append(result.Users, batch.Users...)

		updateHashesFromBatch(batch, userHashes, channelHashes)

		lastDialog := batch.Dialogs[len(batch.Dialogs)-1]
		prevOffsetDate := offsetDate
		prevOffsetID := offsetID

		switch dlg := lastDialog.(type) {
		case *tg.Dialog:
			offsetID = dlg.TopMessage
			offsetDate = messageDate(batch.Messages, dlg.TopMessage)
			offsetPeer = dialogPeerToInput(dlg.Peer, userHashes, channelHashes)
		case *tg.DialogFolder:
			offsetID = dlg.TopMessage
			offsetDate = messageDate(batch.Messages, dlg.TopMessage)
			offsetPeer = dialogPeerToInput(dlg.Peer, userHashes, channelHashes)
		default:
			offsetPeer = &tg.InputPeerEmpty{}
		}

		if offsetDate == dialogFetchZeroOffset {
			offsetDate = prevOffsetDate
		}
		if offsetID == dialogFetchZeroOffset {
			offsetID = prevOffsetID
		}
		if offsetPeer == nil {
			offsetPeer = &tg.InputPeerEmpty{}
		}

		if len(batch.Dialogs) < dialogFetchPageLimit {
			break
		}

		tgruntime.WaitRandomTimeMs(ctx, dialogFetchWaitMinMs, dialogFetchWaitMaxMs)
	}

	return result, nil
}

func normalizeDialogsResponse(resp tg.MessagesDialogsClass) (*tg.MessagesDialogs, error) {
	switch data := resp.(type) {
	case *tg.MessagesDialogs:
		return data, nil
	case *tg.MessagesDialogsSlice:
		return &tg.MessagesDialogs{
			Dialogs:  data.Dialogs,
			Messages: data.Messages,
			Chats:    data.Chats,
			Users:    data.Users,
		}, nil
	case *tg.MessagesDialogsNotModified:
		return nil, errDialogsNotModified
	default:
		return nil, fmt.Errorf("unexpected dialogs response: %T", resp)
	}
}

func updateHashesFromBatch(batch *tg.MessagesDialogs, userHashes, channelHashes map[int64]int64) {
	for _, entity := range batch.Users {
		if user, ok := entity.(*tg.User); ok {
			userHashes[user.ID] = user.AccessHash
		}
	}
	for _, entity := range batch.Chats {
		switch item := entity.(type) {
		case *tg.Channel:
			channelHashes[item.ID] = item.AccessHash
		}
	}
}

func messageDate(messages []tg.MessageClass, id int) int {
	for _, msg := range messages {
		switch item := msg.(type) {
		case *tg.Message:
			if item.ID == id {
				return item.Date
			}
		case *tg.MessageService:
			if item.ID == id {
				return item.Date
			}
		}
	}
	return dialogFetchZeroOffset
}

func dialogPeerToInput(peer tg.PeerClass, userHashes, channelHashes map[int64]int64) tg.InputPeerClass {
	switch entity := peer.(type) {
	case *tg.PeerUser:
		return &tg.InputPeerUser{
			UserID:     entity.UserID,
			AccessHash: userHashes[entity.UserID],
		}
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: entity.ChatID}
	case *tg.PeerChannel:
		return &tg.InputPeerChannel{
			ChannelID:  entity.ChannelID,
			AccessHash: channelHashes[entity.ChannelID],
		}
	default:
		return &tg.InputPeerEmpty{}
	}
}
