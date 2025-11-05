// Package filters содержит структуры и методы для загрузки, хранения и управления
// фильтрами сообщений и их получателями.
// Файл filterengine.go содержит основной движок фильтрации.
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

// Init подготавливает внутреннее состояние FilterEngine:
// - загружает получателй и фильтры
// - оставляет фильтры только с известными получателями (логирует ошибку если фильтр пропускается)
// - оставляет только тех получаетелй, которые используются в фильтрах (логирует предупреждение если получатель не используется)
// и сохраняет мапу с получателями
// - сохраняет список уникальных чатов
func (fe *FilterEngine) Init() error {
	recipients, err := LoadRecipients(fe.recipientsPath)
	if err != nil {
		return fmt.Errorf("failed to load recipients: %w", err)
	}
	recipientsMap := make(map[RecipientID]Recipient)
	for _, r := range recipients {
		recipientsMap[r.ID] = r
	}

	filters, err := LoadFilters(fe.filtersPath)
	if err != nil {
		return fmt.Errorf("failed to load filters: %w", err)
	}

	// фильтруем фильтры с неизвестными получателями
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

	// проверяем используемых получателей
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

	fe.mu.Lock()
	defer fe.mu.Unlock()
	fe.filters = validFilters
	fe.recipientsMap = recipientsMap
	fe.uniqueChats = uniqueChats(validFilters)
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
	Result     Result
}

// ProcessMessage прогоняет сообщение по всем фильтрам из конфигурации и собирает
// список сработавших фильтров для текущего получателя (peer).
// Логика:
//   - peer нормализуется в числовой идентификатор через GetPeerID;
//   - фильтр учитывается только если peer присутствует в Filter.Chats;
//   - entities переданы на будущее (ссылки, хэштеги и т. п.) и сейчас не используются;
//   - порядок результатов соответствует порядку фильтров в конфиге;
//   - пустой текст сообщения допустим: все include‑условия должны его выдержать, чтобы фильтр сработал.
func (fe *FilterEngine) ProcessMessage(
	entities tg.Entities,
	msg *tg.Message,
) []FilterMatchResult {
	if msg == nil {
		return nil
	}
	peerKey := tgutil.GetPeerID(msg.PeerID)

	var results []FilterMatchResult

	for _, f := range fe.GetFilters() {
		hasChat := slices.Contains(f.Chats, peerKey)
		if !hasChat {
			continue
		}
		res := MatchMessage(msg.Message, f)
		if res.Matched {
			fe.mu.RLock()
			var recs []Recipient
			for _, recID := range f.Notify.Recipients {
				if r, ok := fe.recipientsMap[RecipientID(recID)]; ok {
					recs = append(recs, r)
				}
			}
			fe.mu.RUnlock()

			results = append(results, FilterMatchResult{
				Filter:     f,
				Recipients: recs,
				Result:     res,
			})
		}
	}
	return results
}
