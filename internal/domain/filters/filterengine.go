// Package filters содержит новую систему фильтрации сообщений с детерминированной логикой
// фильтров. Файл filterengine.go содержит основной движок фильтрации.
package filters

import (
	"fmt"
	"slices"
	"sync"
	"telegram-userbot/internal/domain/tgutil"
	"telegram-userbot/internal/infra/logger"

	"github.com/gotd/td/tg"
)

// FilterEngine хранит загруженные фильтры и обеспечивает потокобезопасный доступ к ним.
type FilterEngine struct {
	filtersPath    string
	recipientsPath string
	filters        []Filter
	recipientsMap  map[RecipientID]Recipient
	uniqueChats    []int64
	mu             sync.RWMutex
}

func NewFilterEngine(filtersPath string, recipientsPath string) *FilterEngine {
	return &FilterEngine{
		filtersPath:    filtersPath,
		recipientsPath: recipientsPath,
	}
}

// Init подготавливает внутреннее состояние FilterEngine
func (fe *FilterEngine) Init() error {
	recipients, err := LoadRecipients(fe.recipientsPath)
	if err != nil {
		return fmt.Errorf("failed to load recipients: %w", err)
	}

	// Создаем мапу получателей
	recipientsMap := make(map[RecipientID]Recipient, len(recipients))
	for _, r := range recipients {
		recipientsMap[r.ID] = r
	}

	// Загружаем фильтры (уже валидированные и с предкомпилированными паттернами)
	filters, err := LoadFilters(fe.filtersPath)
	if err != nil {
		return fmt.Errorf("failed to load filters: %w", err)
	}

	// Фильтруем фильтры с неизвестными получателями
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

	// Проверяем используемых получателей
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

	// Однократный захват мьютекса для записи всех данных
	fe.mu.Lock()
	fe.filters = validFilters
	fe.recipientsMap = recipientsMap
	fe.uniqueChats = uniqueChats(validFilters)
	fe.mu.Unlock()

	logger.Infof("FilterEngine initialized: %d filters, %d recipients, %d unique chats",
		len(validFilters), len(recipientsMap), len(fe.uniqueChats))

	return nil
}

// uniqueChats возвращает срез уникальных идентификаторов чатов из всех фильтров.
func uniqueChats(filters []Filter) []int64 {
	uniqueChatsMap := make(map[int64]struct{})
	for _, f := range filters {
		for _, chatID := range f.Chats {
			uniqueChatsMap[chatID] = struct{}{}
		}
	}
	uniqueChats := make([]int64, 0, len(uniqueChatsMap))
	for chatID := range uniqueChatsMap {
		uniqueChats = append(uniqueChats, chatID)
	}
	return uniqueChats
}

// GetFilters возвращает актуальную копию среза фильтров. Благодаря RLock
// и копированию наружу, вызывающий код не может повредить внутреннее состояние.
func (fe *FilterEngine) GetFilters() []Filter {
	fe.mu.RLock()
	defer fe.mu.RUnlock()
	return fe.filters
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
	filters := fe.filters
	recipientsMapCopy := fe.recipientsMap
	fe.mu.RUnlock()

	var results []FilterMatchResult

	for _, f := range filters {
		hasChat := slices.Contains(f.Chats, peerKey)
		if !hasChat {
			continue
		}

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
