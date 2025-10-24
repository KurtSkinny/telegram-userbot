// Package concurrency — вспомогательная инфраструктура конкурентного исполнения.
// Данный файл содержит Deduplicator — потокобезопасный кэш «недавно видели»,
// который подавляет повторную обработку событий в пределах заданного окна времени.
// Используется поверх входящих апдейтов Telegram, чтобы не запускать бизнес‑логику
// повторно при приходе идентичных апдейтов или частых правках сообщения.

package concurrency

import (
	"context"
	"fmt"
	"sync"
	"time"

	"telegram-userbot/internal/infra/logger"
)

// Deduplicator хранит «сигнатуры» недавно обработанных событий и решает,
// считать ли очередное событие повтором в рамках заданного окна.
// Ключ формируется как `<chatID>:<msgID>:<editDate>`; изменение editDate при
// правке сообщения приводит к новой сигнатуре. Структура потокобезопасна.
type Deduplicator struct {
	mu     sync.Mutex           // mu защищает доступ к карте seen из параллельных горутин.
	seen   map[string]time.Time // key -> expireAt; хранит срок годности записи для быстрой проверки повторов.
	window time.Duration        // длительность окна дедупликации; до истечения expireAt событие считается повтором.

	runMu  sync.Mutex         // runMu защищает старт/остановку фоновой горутины очистки.
	cancel context.CancelFunc // cancel завершает цикл очистки, если он был запущен.
	wg     sync.WaitGroup     // wg дожидается завершения фоновой горутины при остановке.
}

// NewDeduplicator создаёт кэш подавления повторов с окном `windowSec` секунд.
// Нулевое значение означает «повторов нет» только мгновенно на текущем тике времени,
// поэтому обычно имеет смысл задавать положительное окно (например 60 сек).
func NewDeduplicator(windowSec int) *Deduplicator {
	return &Deduplicator{
		seen:   make(map[string]time.Time),
		window: time.Duration(windowSec) * time.Second,
	}
}

// Start поднимает фоновую горутину очистки устаревших ключей. Повторные вызовы
// безопасны и игнорируются. Если передан nil‑контекст, запуск отменяется.
func (d *Deduplicator) Start(ctx context.Context) {
	if ctx == nil {
		return
	}

	d.runMu.Lock()
	defer d.runMu.Unlock()

	if d.cancel != nil {
		return
	}

	// Развязываем жизненный цикл очистки от внешнего контекста через CancelFunc.
	runCtx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	d.wg.Go(func() {
		// Раз в минуту вычищаем просроченные записи, чтобы карта не росла бесконечно.
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				d.DedupCleanup()
			}
		}
	})
}

// Stop корректно завершает фоновую очистку и дожидается её окончания, гарантируя,
// что во время остановки не происходит конкурирующей модификации карты.
func (d *Deduplicator) Stop() {
	d.runMu.Lock()
	cancel := d.cancel
	d.cancel = nil
	d.runMu.Unlock()

	if cancel == nil {
		return
	}

	cancel()
	d.wg.Wait()
}

// DedupSeen сообщает, видели ли уже событие с сигнатурой `<chatID>:<msgID>:<editDate>`
// в пределах окна. Возвращает true, если запись ещё актуальна (повтор), иначе
// регистрирует новую запись с истечением через d.window и возвращает false.
func (d *Deduplicator) DedupSeen(chatID int64, msgID int, editDate int) bool {
	key := fmt.Sprintf("%d:%d:%d", chatID, msgID, editDate)

	// editDate == 0 для «первой версии» сообщения; при правке появится новое значение,
	// что естественным образом снимает дедупликацию для изменённого текста.

	d.mu.Lock()
	defer d.mu.Unlock()

	// Сравниваем текущее время с запланированным expireAt для ключа.
	now := time.Now()
	if exp, ok := d.seen[key]; ok && now.Before(exp) {
		logger.Debug(fmt.Sprintf("DEDUP SEEN: %v", key))
		return true
	}
	d.seen[key] = now.Add(d.window)
	return false
}

// DedupCleanup удаляет из карты все записи с истёкшим сроком. Метод потокобезопасен
// и может вызываться как фоново (через Start), так и синхронно по необходимости.
func (d *Deduplicator) DedupCleanup() {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Линейный проход по карте; объём обычно мал благодаря периодической очистке.
	now := time.Now()
	for k, exp := range d.seen {
		if now.After(exp) {
			delete(d.seen, k)
		}
	}
}
