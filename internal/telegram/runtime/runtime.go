// Package telegramruntime — вспомогательные утилиты рантайма для userbot.
// В этом файле: ожидания с псевдослучайной длительностью, уважающие контекст отмены,
// дефолтные окна ожидания и тонкости корректного обращения с таймерами.

package telegramruntime

import (
	"context"
	"math/rand/v2"
	"telegram-userbot/internal/logger"
	"time"
)

const (
	// defaultWaitMinMs — минимальная длительность ожидания по умолчанию (мс), используется в WaitRandomTime().
	defaultWaitMinMs = 1111
	// defaultWaitMaxMs — максимальная длительность ожидания по умолчанию (мс). TODO: опечатка в имени, переименовать в defaultWaitMaxMs при следующем изменении API.
	defaultWaitMaxMs = 3333
)

// WaitRandomTimeMs блокирует текущую горутину на случайный интервал из [minMs, maxMs).
// Таймер немедленно отменяется при ctx.Done(). Поведение на краях:
//   - если minMs==maxMs — ждём ровно это значение;
//   - если обе границы равны нулю — используем дефолтные окна (defaultWaitMinMs..cefaultWaitMaxMs);
//   - если minMs<=0 или maxMs<minMs — логируем ошибку и выходим без ожидания.
func WaitRandomTimeMs(ctx context.Context, minMs, maxMs int) {
	// Нормализация входных параметров: дефолты и валидация.
	switch {
	case minMs == 0 && maxMs == 0:
		minMs = defaultWaitMinMs
		maxMs = defaultWaitMaxMs
		// дефолтное окно ожидания
	case minMs <= 0:
		logger.Error("WaitRandomTimeMs: wait time <= 0")
		return
	case maxMs < minMs:
		logger.Error("WaitRandomTimeMs: max < min")
		return
	}

	// Равномерный выбор из полуинтервала [minMs, maxMs): верхняя граница исключена.
	delta := maxMs
	if maxMs > minMs {
		delta = rand.IntN((maxMs - minMs)) + minMs // #nosec G404
	}
	delay := time.Duration(delta) * time.Millisecond

	// Создаём одноразовый таймер на выбранную задержку; по выходу обязательно Stop().
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		// Если контекст отменён до срабатывания таймера — останавливаем и осушаем канал, чтобы не протекало в следующий select.
		if !timer.Stop() {
			<-timer.C
		}
		return
	case <-timer.C:
		return
	}
}

// WaitRandomTime — удобная обёртка, использующая дефолтные окна ожидания.
// Эквивалентно WaitRandomTimeMs(ctx, 0, 0).
func WaitRandomTime(ctx context.Context) {
	WaitRandomTimeMs(ctx, 0, 0)
}

// NOTE: устаревший код статусов оставлен для справки; актуальная реализация в internal/infra/telegram/status.
// // lastStatusOnline хранит момент последнего успешного обновления статуса, что
// // позволяет зафиксировать минимальный интервал между запросами к API.
// var (
// 	lastStatusOnline time.Time
// )

// // SetStatusOnline отправляет в Telegram обновление статуса «онлайн», но не чаще
// // одного раза в минуту. Это позволяет имитировать присутствие пользователя без
// // лишней нагрузки на API. Контекст ctx используется для отмены запроса, а api —
// // Telegram-клиент из библиотеки gotd.
// func SetStatusOnline(ctx context.Context, api *tg.Client) {
// 	now := time.Now()
// 	if now.Sub(lastStatusOnline) < 1*time.Minute {
// 		// слишком рано повторять — пропускаем
// 		return
// 	}

// 	if _, err := api.AccountUpdateStatus(ctx, false); err != nil {
// 		logger.Errorf("SetStatusOnline: %v", err)
// 	}

// 	lastStatusOnline = now
// }
