// Package recipients — доменные модели управления получателями уведомлений.
// Здесь описаны получатели, резолверы и менеджер для централизованной
// обработки всей логики работы с получателями уведомлений.
package recipients

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"telegram-userbot/internal/infra/logger"
)

// Recipient — полное описание получателя из recipients.json
type Recipient struct {
	ID       string   `json:"-"`    // ключ из JSON (заполняется при парсинге)
	Kind     string   `json:"kind"` // user|chat|channel
	PeerID   int64    `json:"peer_id"`
	Note     string   `json:"note"`
	TZ       string   `json:"tz"`
	Schedule []string `json:"schedule"`
}

// RecipientsConfig — корневая структура recipients.json
type RecipientsConfig struct {
	Recipients map[string]Recipient `json:"recipients"`
}

// RecipientManager — управляет загрузкой, валидацией и доступом к получателям
type RecipientManager struct {
	filePath   string
	recipients map[string]Recipient // ID -> Recipient
	mu         sync.RWMutex
}

// NewRecipientManager создает менеджер получателей
func NewRecipientManager(filePath string) *RecipientManager {
	return &RecipientManager{
		filePath:   filePath,
		recipients: make(map[string]Recipient),
	}
}

// ResolvedRecipient — упрощенная структура для создания Job
type ResolvedRecipient struct {
	Kind   string // user|chat|channel
	PeerID int64
}

// GetByID возвращает получателя по ID, ok=false если не найден
func (rm *RecipientManager) GetByID(id string) (Recipient, bool) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	r, ok := rm.recipients[id]
	return r, ok
}

// GetByIDs возвращает список получателей по списку ID
// Пропускает несуществующие ID с логированием warning
func (rm *RecipientManager) GetByIDs(ids []string) []Recipient {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	var result []Recipient
	for _, id := range ids {
		if r, ok := rm.recipients[id]; ok {
			result = append(result, r)
		} else {
			logger.Warnf("Recipient with ID '%s' not found", id)
		}
	}
	return result
}

// ResolveToTargets преобразует список recipient_id в структуру для создания Job
// Возвращает список готовых получателей с типом и peer_id
func (rm *RecipientManager) ResolveToTargets(ids []string) []ResolvedRecipient {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	var result []ResolvedRecipient
	for _, id := range ids {
		if r, ok := rm.recipients[id]; ok {
			result = append(result, ResolvedRecipient{
				Kind:   r.Kind,
				PeerID: r.PeerID,
			})
		} else {
			logger.Warnf("Recipient with ID '%s' not found", id)
		}
	}
	return result
}

// ValidateIDs проверяет существование всех ID, возвращает ошибку если хотя бы один не найден
func (rm *RecipientManager) ValidateIDs(ids []string) error {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	for _, id := range ids {
		if _, ok := rm.recipients[id]; !ok {
			return fmt.Errorf("recipient with ID '%s' not found", id)
		}
	}
	return nil
}

// GetAll возвращает все получатели
func (rm *RecipientManager) GetAll() map[string]Recipient {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	result := make(map[string]Recipient)
	for k, v := range rm.recipients {
		result[k] = v
	}
	return result
}

// Load загружает и валидирует recipients.json
// Возвращает ошибку, если файл не найден или невалиден
func (rm *RecipientManager) Load() error {
	data, err := os.ReadFile(filepath.Clean(rm.filePath))
	if err != nil {
		return fmt.Errorf("failed to read recipients json: %w", err)
	}

	var config RecipientsConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to unmarshal recipients json: %w", err)
	}

	// Создаем мапу для проверки дубликатов kind:peer_id
	kindPeerMap := make(map[string][]string)
	uniqueRecipients := make(map[string]Recipient)

	// Валидируем каждого получателя
	for id, recipient := range config.Recipients {
		// Валидируем peer_id
		if recipient.PeerID <= 0 {
			logger.Errorf("Recipient '%s' has invalid peer_id: %d, skipping", id, recipient.PeerID)
			continue
		}

		// Устанавливаем kind по умолчанию, если пустой
		if recipient.Kind == "" {
			recipient.Kind = "user"
		}

		// Проверяем, что kind допустим
		if recipient.Kind != "user" && recipient.Kind != "chat" && recipient.Kind != "channel" {
			logger.Errorf("Recipient '%s' has invalid kind: '%s', skipping", id, recipient.Kind)
			continue
		}

		// Валидируем tz, если указан
		if recipient.TZ != "" {
			if _, err := ParseLocation(recipient.TZ); err != nil {
				logger.Errorf("Recipient '%s' has invalid timezone: '%s', skipping", id, recipient.TZ)
				continue
			}
		}

		// Валидируем schedule, если указан
		if len(recipient.Schedule) > 0 {
			validSchedule := []string{}
			for _, entry := range recipient.Schedule {
				if IsValidScheduleEntry(entry) {
					validSchedule = append(validSchedule, entry)
				} else {
					logger.Errorf("Recipient '%s' has invalid schedule entry: '%s', skipping", id, entry)
				}
			}
			recipient.Schedule = validSchedule
		}

		// Проверка дубликатов по kind+peer_id
		kindPeerKey := recipient.Kind + ":" + strconv.FormatInt(recipient.PeerID, 10)
		if existingIDs, exists := kindPeerMap[kindPeerKey]; exists {
			logger.Warnf("Recipient '%s' has same kind:peer_id ('%s') as existing recipient(s): %v", id, kindPeerKey, existingIDs)
		} else {
			kindPeerMap[kindPeerKey] = []string{id}
		}

		// Сохраняем ID в структуре
		recipient.ID = id
		uniqueRecipients[id] = recipient
	}

	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.recipients = uniqueRecipients

	return nil
}

// IsValidScheduleEntry проверяет формат времени HH:MM и диапазоны часов/минут.
// Это копия функции из config пакета
func IsValidScheduleEntry(value string) bool {
	if len(value) != 5 || value[2] != ':' {
		return false
	}
	hour, err := strconv.Atoi(value[:2])
	if err != nil {
		return false
	}
	minute, err := strconv.Atoi(value[3:])
	if err != nil {
		return false
	}
	if hour < 0 || hour > 23 {
		return false
	}
	if minute < 0 || minute > 59 {
		return false
	}
	return true
}

// ParseLocation разбирает либо IANA‑таймзону (например, "Europe/Moscow"),
// либо UTC‑смещение (например, "+03:00", "-0700", "UTC+3", "GMT-04:30").
// Это копия функции из config пакета
func ParseLocation(value string) (*time.Location, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil, fmt.Errorf("empty timezone")
	}
	// Try IANA first.
	if loc, err := time.LoadLocation(v); err == nil {
		return loc, nil
	}
	// Try to parse UTC offset forms.
	if loc, ok := parseUTCOffsetToLocation(v); ok {
		return loc, nil
	}
	return nil, fmt.Errorf("invalid timezone %q: not an IANA name or UTC offset", value)
}

// parseUTCOffsetToLocation парсит строки вида "+03:00", "-0700", "UTC+3", "GMT-04:30" или "Z".
// Возвращает фиксированную таймзону и ok=true при успешном разборе.
// Это копия функции из config пакета
func parseUTCOffsetToLocation(value string) (*time.Location, bool) {
	v := strings.TrimSpace(strings.ToUpper(value))
	if v == "Z" || v == "UTC" || v == "GMT" {
		return time.FixedZone("UTC+00:00", 0), true
	}
	// Normalize optional UTC/GMT prefix
	v = strings.TrimPrefix(v, "UTC")
	v = strings.TrimPrefix(v, "GMT")
	v = strings.TrimSpace(v)
	// Patterns: +HH, -HH, +HHMM, -HHMM, +HH:MM, -HH:MM
	re := regexp.MustCompile(`^([+-])\s*(\d{1,2})(?::?(\d{2}))?$`)
	m := re.FindStringSubmatch(v)
	if m == nil {
		return nil, false
	}
	sign := 1
	if m[1] == "-" {
		sign = -1
	}
	hourStr := m[2]
	minStr := m[3]
	hours, err := strconv.Atoi(hourStr)
	if err != nil {
		return nil, false
	}
	mins := 0
	if minStr != "" {
		var err2 error
		mins, err2 = strconv.Atoi(minStr)
		if err2 != nil {
			return nil, false
		}
	}
	if hours < 0 || hours > 14 || mins < 0 || mins > 59 {
		return nil, false
	}
	offset := sign * ((hours * 60 * 60) + (mins * 60))
	name := fmt.Sprintf("UTC%+03d:%02d", sign*hours, mins)
	return time.FixedZone(name, offset), true
}

// RecipientTargets — старая структура для обратной совместимости
// Не используется в новом формате, но сохранена для совместимости с остальной частью системы
type RecipientTargets struct {
	Users    []int64 `json:"users"`
	Chats    []int64 `json:"chats"`
	Channels []int64 `json:"channels"`
}

// UnmarshalJSON реализует особую десериализацию для RecipientTargets:
// пытается распарсить вход как []int64; если не получилось — как объект
// {users, chats, channels}. Нули и повторы удаляются, чтобы избежать кривых
// адресатов в рантайме при доставке уведомлений.
func (r *RecipientTargets) UnmarshalJSON(b []byte) error {
	// Попытка распарсить как старый формат: []int64
	var flat []int64
	if err := json.Unmarshal(b, &flat); err == nil {
		r.Users = uniqueNonZero(flat)
		r.Chats = nil
		r.Channels = nil
		return nil
	}
	// Пробуем новый формат: объект
	var tmp struct {
		Users    []int64 `json:"users"`
		Chats    []int64 `json:"chats"`
		Channels []int64 `json:"channels"`
	}
	if err := json.Unmarshal(b, &tmp); err != nil {
		return err
	}
	r.Users = uniqueNonZero(tmp.Users)
	r.Chats = uniqueNonZero(tmp.Chats)
	r.Channels = uniqueNonZero(tmp.Channels)
	return nil
}

// uniqueNonZero удаляет нули и дубликаты из списка ID, сохраняя порядок
// первого появления. Возвращает nil, если в результате список пуст.
//
// Причина: нулевые/повторные ID часто возникают при ручном редактировании
// конфигов и приводят к «мусору» при маршрутизации уведомлений.
func uniqueNonZero(in []int64) []int64 {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if v == 0 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
