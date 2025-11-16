// Package filters содержит новую систему фильтрации сообщений с детерминированной логикой
// фильтров. Файл filterengine.go содержит основной движок фильтрации.
package filters

import (
	"fmt"
	"sync"
	"telegram-userbot/internal/domain/tgutil"
	"telegram-userbot/internal/infra/logger"

	"github.com/gotd/td/tg"
)

// FilterEngine хранит загруженные фильтры и обеспечивает потокобезопасный доступ к ним.
type FilterEngine struct {
	filtersPath    string
	recipientsPath string
	// filters        []Filter
	chatFilters   map[int64][]Filter
	recipientsMap map[RecipientID]Recipient
	uniqueChats   []int64
	mu            sync.RWMutex
}

func NewFilterEngine(filtersPath string, recipientsPath string) *FilterEngine {
	return &FilterEngine{
		filtersPath:    filtersPath,
		recipientsPath: recipientsPath,
	}
}

// Load подготавливает внутреннее состояние FilterEngine
func (fe *FilterEngine) Load() error {
	recipients, err := LoadRecipients(fe.recipientsPath)
	if err != nil {
		return fmt.Errorf("failed to load recipients: %w", err)
	}

	recipientsMap := make(map[RecipientID]Recipient, len(recipients))
	for _, r := range recipients {
		recipientsMap[r.ID] = r
	}

	filters, err := LoadFilters(fe.filtersPath)
	if err != nil {
		return fmt.Errorf("failed to load filters: %w", err)
	}

	// Фильтруем фильтры с неизвестными получателями
	validFilters := getValidFilter(filters, recipientsMap)

	chatFilters := make(map[int64][]Filter)
	for _, f := range validFilters {
		for _, chatID := range f.Chats {
			chatFilters[chatID] = append(chatFilters[chatID], f)
		}
	}

	uniqueChats := make([]int64, 0, len(chatFilters))
	for chatID := range chatFilters {
		uniqueChats = append(uniqueChats, chatID)
	}

	// Однократный захват мьютекса для записи всех данных
	fe.mu.Lock()
	fe.recipientsMap = recipientsMap
	fe.chatFilters = chatFilters
	fe.uniqueChats = uniqueChats
	fe.mu.Unlock()

	// Проверяем используемых получателей и пишем в лог
	checkUsedRecipients(validFilters, recipientsMap)

	logger.Infof("FilterEngine initialized: %d filters, %d recipients, %d unique chats",
		len(validFilters), len(recipientsMap), len(fe.uniqueChats))

	return nil
}

func checkUsedRecipients(validFilters []Filter, recipientsMap map[RecipientID]Recipient) {
	usedRecipients := make(map[RecipientID]struct{})
	for _, f := range validFilters {
		for _, recID := range f.Notify.Recipients {
			usedRecipients[RecipientID(recID)] = struct{}{}
		}
	}
	for recID := range recipientsMap {
		if _, used := usedRecipients[recID]; !used {
			logger.Warnf("recipient %s is not used in any filter", recID)
		}
	}
}

func getValidFilter(filters []Filter, recipientsMap map[RecipientID]Recipient) []Filter {
	validFilters := make([]Filter, 0, len(filters))
	for _, f := range filters {
		allRecipientsKnown := true
		for _, recID := range f.Notify.Recipients {
			if _, ok := recipientsMap[RecipientID(recID)]; !ok {
				logger.Errorf("filter %s references unknown recipient %s, skipping filter", f.ID, recID)
				allRecipientsKnown = false
				break
			}
		}
		if allRecipientsKnown {
			validFilters = append(validFilters, f)
		}
	}
	return validFilters
}

// GetUniqueChats возвращает копию множества всех чатов, встречающихся во всех
// фильтрах. Отдаётся новый срез, чтобы внешний код не мог модифицировать кеш.
func (fe *FilterEngine) GetUniqueChats() []int64 {
	fe.mu.RLock()
	defer fe.mu.RUnlock()
	// копия, чтобы не отдавать внутренний срез наружу
	result := make([]int64, len(fe.uniqueChats))
	copy(result, fe.uniqueChats)
	return result
}

// FilterMatchResult связывает фильтр из конфигурации и его результат.
// Используется для логирования и дальнейшей бизнес‑обработки
// (например, выбор действия согласно типу фильтра).
type FilterMatchResult struct {
	Filter     Filter
	Recipients []Recipient
	Result     FilterResult
}

// FilterResult — результат вычисления фильтра.
type FilterResult struct {
	Matched     bool            // Сработал ли фильтр
	ResultType  MatchResultType // Тип результата
	MatchedNode Node            // Узел AST, который дал срабатывание
}

// MatchResultType — тип результата фильтрации.
type MatchResultType int

const (
	// DROP — сработал deny, сообщение выбрасывается
	Drop MatchResultType = iota
	// ALLOW_MATCH — deny не сработал и allow дал истину
	AllowMatch
	// PASS_THROUGH — deny не сработал, allow пустой → считаем нужным
	PassThrough
	// NO_MATCH — deny не сработал, но allow есть и не совпал
	NoMatch
)

// String возвращает строковое представление типа результата
func (mrt MatchResultType) String() string {
	switch mrt {
	case Drop:
		return "DROP"
	case AllowMatch:
		return "ALLOW_MATCH"
	case PassThrough:
		return "PASS_THROUGH"
	case NoMatch:
		return "NO_MATCH"
	default:
		return "UNKNOWN"
	}
}

// ProcessMessage прогоняет сообщение по всем фильтрам из конфигурации и собирает
// список сработавших фильтров для текущего получателя (peer).
// Логика:
//   - peer нормализуется в числовой идентификатор через GetPeerID;
//   - фильтр учитывается только если peer присутствует в Filter.Chats;
//   - entities переданы на будущее (ссылки, хэштеги и т. п.) и сейчас не используются;
//   - порядок результатов соответствует порядку фильтров в конфиге;
//   - пустой текст сообщения допустим: все include‑условия должны его выдержать, чтобы фильтр сработал.
//
// ProcessMessage с оптимизированной работой мьютекса
func (fe *FilterEngine) ProcessMessage(
	entities tg.Entities,
	msg *tg.Message,
) []FilterMatchResult {
	if msg == nil {
		return nil
	}
	peerKey := tgutil.GetPeerID(msg.PeerID)

	fe.mu.RLock()
	filtersForChat, exists := fe.chatFilters[peerKey]
	if !exists {
		fe.mu.RUnlock()
		return nil
	}
	recipientsMapCopy := fe.recipientsMap
	fe.mu.RUnlock()

	var results []FilterMatchResult

	for _, f := range filtersForChat {
		res := MatchMessage(msg.Message, f)
		if res.Matched {
			var recs []Recipient
			for _, recID := range f.Notify.Recipients {
				if r, ok := recipientsMapCopy[RecipientID(recID)]; ok {
					recs = append(recs, r)
				}
			}

			results = append(results, FilterMatchResult{
				Filter:     f,
				Recipients: recs,
				Result:     res,
			})
		}
	}

	return results
}
