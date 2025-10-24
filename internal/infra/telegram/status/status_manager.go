// Package status инкапсулирует логику поддержания онлайн-статуса пользовательского
// аккаунта Telegram. Менеджер получает сигналы активности, управляет переходами
// online/offline и централизует вызовы AccountUpdateStatus через gotd/td (tg.Client).
// Бизнес-назначение: обеспечить единообразное поведение статуса и избежать гонок,
// когда разные части приложения пытаются менять статус одновременно.
package status

import (
	"context"
	"sync"

	// "telegram-userbot/internal/infra/telegram/connection"

	"github.com/gotd/td/tg"
)

// StatusManager управляет онлайн-статусом пользователя. Это процессный синглтон, который
// принимает сигналы активности и синхронно/асинхронно инициирует AccountUpdateStatus.
// Он также следит за тайм-аутом простоя, чтобы вовремя уйти в offline, если нет пингов.
// Через него все подсистемы сообщают о «жизни» клиента, а он гарантирует консистентность вызовов API.
type StatusManager struct {
	api    *tg.Client    // Клиент gotd для вызовов AccountUpdateStatus (online/offline/typing и т.п.).
	pingCh chan int      // Буферизованный канал (1) для сигналов активности; всплески схлопываются.
	doneCh chan struct{} // Закрывается после завершения run(); на нём ждёт Shutdown().
}

// Глобальное хранение синглтона менеджера и его cancel-функции. Доступ защищён mutex-ом.
var (
	manager *StatusManager // manager хранит глобальный экземпляр менеджера статуса для всего процесса

	statusMu     sync.Mutex
	statusCancel context.CancelFunc
)

// Init создаёт и запускает менеджер статуса. Безопасна к повторным вызовам: второй и далее — no-op.
// Контекст управляет жизненным циклом фоновой горутины. Через api выполняются вызовы AccountUpdateStatus.
func Init(ctx context.Context, api *tg.Client) {
	statusMu.Lock()
	defer statusMu.Unlock()

	if manager != nil {
		return
	}

	runCtx, cancel := context.WithCancel(ctx)
	// Отдельный под-контекст, чтобы можно было целенаправленно гасить менеджер из Shutdown().
	manager = &StatusManager{
		api:    api,
		pingCh: make(chan int, 1),
		doneCh: make(chan struct{}),
	}
	// pingCh имеет размер 1, поэтому частые пинги будут схлопываться до одного непроцессенного сигнала.
	statusCancel = cancel
	go manager.run(runCtx)
}

// Shutdown останавливает менеджер: Cancel контекста → ожидание закрытия doneCh. Повторные вызовы — безопасны.
func Shutdown() {
	statusMu.Lock()
	m := manager
	cancel := statusCancel
	manager = nil
	statusCancel = nil
	statusMu.Unlock()

	if m == nil {
		return
	}

	if cancel != nil {
		cancel()
	}
	// Ждём корректного завершения run(), чтобы не оставить висящие таймеры/горрутины.
	<-m.doneCh
}
