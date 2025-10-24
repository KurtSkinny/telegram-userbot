// Package concurrency — утилиты для безопасного конкурентного исполнения.
// В этом файле реализован Debouncer — механизм «сглаживания» повторяющихся событий
// по идентификатору сообщения. Он откладывает выполнение функции до тех пор, пока
// активность по тому же msgID не утихнет, и запускает обработку один раз — по
// «последнему слову».
//
// Применение: снятие нагрузки с обработчиков входящих апдейтов Telegram при частых
// правках одного и того же сообщения. Гарантии: потокобезопасность, отсутствие
// сетевых вызовов под мьютексом, выполнение отложенных функций вне критической секции.

package concurrency

import (
	"context"
	"sync"
	"time"
)

// Debouncer группирует повторяющиеся действия по msgID и запускает их только
// один раз после паузы. Структура потокобезопасна, поэтому её можно переиспользовать
// несколькими горутинами без дополнительной синхронизации.
type Debouncer struct {
	mu      sync.Mutex           // mu защищает доступ к pending и гарантирует потокобезопасность.
	pending map[int]pendingEntry // pending хранит активные таймеры и соответствующие функции по msgID.
	timeout time.Duration        // timeout определяет задержку между последним событием и выполнением fn.

	runMu  sync.Mutex         // runMu отвечает за запуск/остановку фонового наблюдателя.
	ctx    context.Context    // ctx хранит активный контекст, используемый для отмены работы дебаунсера.
	cancel context.CancelFunc // cancel инициирует остановку и немедленное выполнение накопленных функций.
	wg     sync.WaitGroup     // wg позволяет дождаться завершения горутины watchCancel.
}

// pendingEntry сохраняет таймер и отложенный колбэк, чтобы при форсированной остановке их можно было вызвать вручную.
type pendingEntry struct {
	timer *time.Timer
	fn    func()
}

// NewDebouncer создаёт дебаунсер с заданной задержкой между последним событием
// и исполнением функции. Таймаут задаётся в миллисекундах. Конструктор только
// инициализирует структуру; привязка к жизненному циклу выполняется через Start.
func NewDebouncer(timeoutMS int) *Debouncer {
	return &Debouncer{
		pending: make(map[int]pendingEntry),
		timeout: time.Duration(timeoutMS) * time.Millisecond,
	}
}

// Start привязывает Debouncer к контексту и запускает фоновую горутину,
// которая ждёт отмены и аккуратно дренирует накопленные вызовы. Повторные
// вызовы безопасно игнорируются; nil‑контекст означает «не запускать».
func (d *Debouncer) Start(ctx context.Context) {
	if ctx == nil {
		return
	}
	d.runMu.Lock()
	// Сериализуем Start/Stop, чтобы не допустить гонок за cancel/ctx.
	defer d.runMu.Unlock()

	d.mu.Lock()
	// Идемпотентность запуска: если уже запущены, выходим.
	if d.cancel != nil {
		d.mu.Unlock()
		return
	}
	// Создаём дочерний контекст с возможностью явной отмены через Stop().
	runCtx, cancel := context.WithCancel(ctx)
	d.ctx = runCtx
	d.cancel = cancel
	d.mu.Unlock()

	// Стартуем наблюдателя, который при отмене контекста дренирует очереди.
	d.wg.Go(func() { d.waitCancel(runCtx) })
}

// Stop останавливает дебаунсер: отменяет контекст, дожидается завершения
// фоновой горутины и синхронно выполняет все отложенные функции. Гарантирует,
// что после возврата не останется активных таймеров/ссылок на внешние ресурсы.
func (d *Debouncer) Stop() {
	d.runMu.Lock()
	// Сериализуем Stop/Start, чтобы захватить и обнулить cancel единообразно.
	var cancel context.CancelFunc
	d.mu.Lock()
	// Забираем текущий cancel и обнуляем поля под локом, чтобы Do/Start не увидели
	// полузакрытое состояние.
	cancel = d.cancel
	d.cancel = nil
	d.ctx = nil
	d.mu.Unlock()
	d.runMu.Unlock()

	if cancel == nil {
		return
	}
	// Сигнализируем горутине наблюдателя завершиться.
	cancel()
	// Дожидаемся корректного завершения watchCancel.
	d.wg.Wait()
	// Выполняем все отложенные операции уже после остановки наблюдателя.
	d.flushPending()
}

// Do регистрирует функцию для msgID и откладывает её запуск на timeout.
// Повторные вызовы для того же msgID перезапускают таймер и заменяют колбэк
// на новый. Если дебаунсер остановлен или контекст отменён, функция выполняется
// немедленно, без ожидания таймаута.
func (d *Debouncer) Do(msgID int, fn func()) {
	d.mu.Lock()

	// Если не запущены или контекст уже отменён — выполняем без отложки.
	if d.ctx == nil || d.ctx.Err() != nil {
		d.mu.Unlock()
		fn()
		return
	}

	if entry, exists := d.pending[msgID]; exists {
		// Перезапускаем окно дебаунса: старый таймер останавливаем, колбэк заменяем.
		if entry.timer != nil {
			entry.timer.Stop()
		}
	}

	// Планируем отложенное выполнение: по истечении timeout вызовем execute(msgID).
	timer := time.AfterFunc(d.timeout, func() {
		d.execute(msgID)
	})
	d.pending[msgID] = pendingEntry{
		timer: timer,
		fn:    fn,
	}
	d.mu.Unlock()
}

// execute извлекает и удаляет отложенный вызов для msgID под локом, затем
// выполняет его вне критической секции. Отсутствие записи считается нормой
// (например, если вызов был уже сброшен Stop()).
func (d *Debouncer) execute(msgID int) {
	var fn func()

	d.mu.Lock()
	if entry, ok := d.pending[msgID]; ok {
		delete(d.pending, msgID)
		fn = entry.fn
	}
	d.mu.Unlock()

	if fn != nil {
		fn()
	}
}

// waitCancel ожидает отмены контекста и инициирует немедленный дренаж всех накопленных функций.
func (d *Debouncer) waitCancel(ctx context.Context) {
	<-ctx.Done()
	d.flushPending()
}

// flushPending синхронно выполняет все накопленные операции. Сначала под локом
// останавливает таймеры, копирует список колбэков и очищает карту, затем
// исполняет функции вне критической секции.
func (d *Debouncer) flushPending() {
	var entries []pendingEntry
	// Делаем снапшот под локом, чтобы исключить гонки, и гасим таймеры, чтобы они не сработали позже.

	d.mu.Lock()
	for id, entry := range d.pending {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		entries = append(entries, entry)
		delete(d.pending, id)
	}
	d.mu.Unlock()

	for _, entry := range entries {
		entry.fn()
	}
}
