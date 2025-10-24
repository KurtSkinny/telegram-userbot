// Package updates / файл notified.go отвечает за идемпотентность рассылки:
// хранит отметки «сообщение × фильтр уже уведомлено», периодически чистит их
// по TTL и обеспечивает безопасную и экономную запись состояния на диск.
// Ключевые задачи:
//   - детерминированный ключ (peerID:msgID:filterID) для быстрых проверок,
//   - TTL‑очистка в фоне (ticker + h.cleanTTL),
//   - ленивый debounced‑флаш при изменениях, с дренированием таймера,
//   - загрузка состояния при старте с отсевом устаревших записей.

package updates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"telegram-userbot/internal/domain/filters"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/storage"

	"github.com/gotd/td/tg"
)

// notifiedSaveDebounce — минимальный интервал между дисковыми сохранениями
// notified.json. Сглаживает частые правки и уменьшает износ FS/IO.
const notifiedSaveDebounce = 10 * time.Second

// persistedNotified — on‑disk формат: key -> unix seconds (UTC).
// key = "<peerID>:<msgID>:<filterID>". Значения мапы конвертируются в time.Time при загрузке.
type persistedNotified map[string]int64

// runNotificationCacheCleaner проходит по h.notified раз в час и удаляет записи
// старше h.cleanTTL. По факту удаления помечает кэш грязным и планирует
// отложенный флаш на диск через scheduleNotifiedSaveLocked(). Останавливается
// по ctx.Done(). Тикер останавливается корректно в defer.
func (h *Handlers) runNotificationCacheCleaner(ctx context.Context) {
	ticker := time.NewTicker(time.Hour) // таймер инициирует проверку раз в час
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-h.cleanTTL)
			// cutoff — нижняя граница допустимого возраста записи.

			h.mu.Lock()
			// Внутри критической секции безопасно удаляем устаревшие ключи.
			deleted := false
			for key, t := range h.notified {
				if t.Before(cutoff) {
					delete(h.notified, key)
					// Если что-то удалили, запланируем отложенную запись на диск.
					deleted = true
				}
			}
			if deleted {
				h.scheduleNotifiedSaveLocked()
			}
			h.mu.Unlock()
		}
	}
}

// loadNotifiedFromDisk загружает JSON‑снимок notified из файла (если он есть),
// отбрасывает записи старше TTL и восстанавливает h.notified. Путь очищается
// через filepath.Clean; отсутствие файла не считается ошибкой (первый запуск).
func (h *Handlers) loadNotifiedFromDisk() {
	path := filepath.Clean(h.notifiedCacheFile)
	// Санитизируем путь; пустой — ничего не делаем.
	if path == "" {
		return
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			// Файл отсутствует — нормальная ситуация на первом запуске.
			return
		}
		logger.Warnf("notified: read failed: %v", readErr)
		return
	}
	var onDisk persistedNotified
	if err := json.Unmarshal(data, &onDisk); err != nil {
		logger.Warnf("notified: unmarshal failed: %v", err)
		return
	}
	now := time.Now()
	cutoff := now.Add(-h.cleanTTL)
	// Отбрасываем устаревшие записи прямо при загрузке, чтобы не раздувать кэш.

	h.mu.Lock()
	for k, ts := range onDisk {
		when := time.Unix(ts, 0)
		if when.Before(cutoff) {
			continue
		}
		h.notified[k] = when
	}
	h.mu.Unlock()
}

// scheduleNotifiedSaveLocked планирует отложенное сохранение notified.json через
// дебаунс. Вызывать строго под h.mu. Первый вызов создаёт таймер и горутину,
// которая по тикеру вызывает flushNotifiedNow(). Повторные вызовы перезапускают
// таймер с корректным дренированием канала, если тик уже произошёл.
func (h *Handlers) scheduleNotifiedSaveLocked() {
	h.notifiedDirty = true
	if h.notifiedSaveTimer == nil {
		t := time.NewTimer(notifiedSaveDebounce)
		// Создаём одноразовый таймер и регистрируем фоновую задачу через h.wg.
		h.notifiedSaveTimer = t
		h.wg.Go(func() {
			<-t.C
			h.flushNotifiedNow()
		})
		return
	}
	if !h.notifiedSaveTimer.Stop() {
		// Дрейним канал таймера, если тик уже успел случиться, чтобы избежать немедленного срабатывания после Reset.
		select {
		case <-h.notifiedSaveTimer.C:
		default:
		}
	}
	h.notifiedSaveTimer.Reset(notifiedSaveDebounce)
}

// flushNotifiedNow выполняет немедленную запись notified.json, если есть изменения.
// Снимок формируется под локом, запись на диск — без блокировки. При ошибках
// маршалинга или записи флаг грязности возвращается, чтобы повторить позже.
func (h *Handlers) flushNotifiedNow() {
	// Снимок под локом
	h.mu.Lock()
	if h.notifiedSaveTimer != nil {
		if !h.notifiedSaveTimer.Stop() {
			select {
			case <-h.notifiedSaveTimer.C:
			default:
			}
		}
		h.notifiedSaveTimer = nil
	}
	if !h.notifiedDirty {
		h.mu.Unlock()
		return
	}
	snapshot := make(persistedNotified, len(h.notified))
	for k, t := range h.notified {
		snapshot[k] = t.Unix()
	}
	path := filepath.Clean(h.notifiedCacheFile)
	h.notifiedDirty = false
	h.mu.Unlock()

	if path == "" {
		return
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		logger.Errorf("notified: marshal failed: %v", err)
		// пометим грязным, чтобы не потерять изменения
		h.mu.Lock()
		h.notifiedDirty = true
		h.mu.Unlock()
		return
	}
	if storeErr := storage.AtomicWriteFile(path, data); storeErr != nil {
		logger.Errorf("notified: save failed: %v", storeErr)
		// повторим сохранение позже
		h.mu.Lock()
		h.notifiedDirty = true
		h.mu.Unlock()
	}
}

// hasNotified проверяет идемпотентность: была ли пара (msg, filterID) уже
// уведомлена ранее. Ключ строится как "<peerID>:<msgID>:<filterID>". Доступ к
// карте защищён h.mu.
func (h *Handlers) hasNotified(msg *tg.Message, filterID string) bool {
	key := fmt.Sprintf("%d:%d:%s", filters.GetPeerID(msg.PeerID), msg.ID, filterID)

	h.mu.Lock()
	_, hasNotified := h.notified[key]
	h.mu.Unlock()

	return hasNotified
}

// markNotified фиксирует, что пара (msg, filterID) уже поставлена в очередь
// уведомлений. Вызывать после успешной постановки, иначе возможны ложные
// «уже отправлено». Помечает кэш как грязный и планирует отложенный флаш на диск.
func (h *Handlers) markNotified(msg *tg.Message, filterID string) {
	key := fmt.Sprintf("%d:%d:%s", filters.GetPeerID(msg.PeerID), msg.ID, filterID)

	h.mu.Lock()
	h.notified[key] = time.Now()
	h.scheduleNotifiedSaveLocked()
	h.mu.Unlock()
}
