// File typing.go: эмуляция статуса "печатает" и дружеский пинок онлайн-статусу.
// Публичные хелперы DoTypingWaitMs/DoTypingWaitChars включают typing, ждут псевдореалистичное
// время и продлевают online через GoOnlineMinMs с небольшим случайным хвостом.
package status

import (
	"context"
	"telegram-userbot/internal/infra/shared"
	tgruntime "telegram-userbot/internal/infra/telegram/runtime"

	"github.com/gotd/td/tg"
)

// Минимальный/максимальный случайный хвост (мс) к окну авто-offline, чтобы не гаснуть сразу после typing.
const deltaMinMs = 5555
const deltaMaxMs = 11111

// DoTypingWaitMs включает статус "печатает" на случайный промежуток [minMs, maxMs] миллисекунд
// и продлевает online на этот же интервал плюс небольшой случайный хвост (deltaMinMs..deltaMaxMs).
// Если менеджер статуса ещё не инициализирован, функция молча возвращает.
func DoTypingWaitMs(ctx context.Context, peer tg.InputPeerClass, minMs, maxMs int) {
	if manager == nil {
		return
	}
	// Добавляем хвост, чтобы аккаунт не выключал online ровно в момент окончания печати.
	deltaMs := shared.Random(deltaMinMs, deltaMaxMs)
	GoOnlineMinMs(deltaMs+minMs, deltaMs+maxMs)
	// Включаем typing через API; ошибки намеренно глотаем — это косметика.
	_, _ = manager.api.MessagesSetTyping(ctx, &tg.MessagesSetTypingRequest{
		Peer:   peer,
		Action: &tg.SendMessageTypingAction{},
	})
	// Ждём указанное окно; уважает ctx и может завершиться досрочно по отмене.
	tgruntime.WaitRandomTimeMs(ctx, minMs, maxMs)
}

// DoTypingWaitChars оценивает длительность печати текста по числу символов и включает typing.
// Базовое окно [wtMin, wtMax] мс увеличивается на charMs за каждый символ, но не более wtMaxAtAll.
// Если менеджер не инициализирован — молча выходим.
func DoTypingWaitChars(ctx context.Context, peer tg.InputPeerClass, text string) {
	if manager == nil {
		return
	}
	// Эвристики длительности печати: базовое окно, стоимость символа и общий максимум.
	const (
		wtMin      = 555
		wtMax      = 1111
		charMs     = 25
		wtMaxAtAll = 5555
	)
	// Считаем по рунам, не по байтам, чтобы эмодзи и кириллица не ломали оценку.
	// min — встроенная с Go 1.21; если таргет старше, в пакете должен быть helper min(int,int).
	textMs := min(len([]rune(text))*charMs, wtMaxAtAll)
	// В итоге ждём базу + поправку на длину текста.
	DoTypingWaitMs(ctx, peer, wtMin+textMs, wtMax+textMs)
}
