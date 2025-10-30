package filters

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"telegram-userbot/internal/domain/recipients"
)

// Match описывает правила совпадения входящих сообщений. Движок фильтрации
// трактует условие как И/ИЛИ/регексп в зависимости от полей:
//   - KeywordsAny: достаточно совпадения по ЛЮБОМУ из слов
//   - KeywordsAll: требуется совпадение по ВСЕМ словам
//   - Regex: проверка произвольным регулярным выражением
//   - ExcludeKeywordsAny / ExcludeRegex: выколотые множества для отрицания
//
// Совпадение считается успехом, если выполняется хотя бы одно «положительное»
// условие и одновременно не срабатывает ни одно исключающее.
type Match struct {
	KeywordsAny        []string `json:"keywords_any"`
	KeywordsAll        []string `json:"keywords_all"`
	Regex              []string `json:"regex"`
	ExcludeKeywordsAny []string `json:"exclude_any"`
	ExcludeRegex       []string `json:"exclude_regex"`
}

// NotifyConfig задает способ доставки уведомления при срабатывании фильтра:
//   - Recipients: список адресатов (пользователи, чаты, каналы),
//   - Forward: пересылать ли оригинал сообщения вместо отправки текста,
//   - Template: форматируемая строка для текстового уведомления.
//
// Конкретная реализация уведомителя (client/bot) определяется через EnvConfig.
type NotifyConfig struct {
	Recipients recipients.RecipientTargets `json:"recipients"`
	Forward    bool                        `json:"forward"`
	Template   string                      `json:"template"`
}

// Filter описывает законченное правило обработки:
//   - ID: стабильный идентификатор правила (для логов, отладки, трекинга),
//   - Chats: из каких источников брать сообщения,
//   - Match: критерии совпадения,
//   - Urgent: маркер «срочных» уведомлений (может влиять на канал доставки),
//   - Notify: параметры маршрутизации уведомлений.
//
// NB: «срочность» никак не интерпретируется здесь в конфиге; семантика — на стороне
// потребителей (например, выбор немедленной отправки вместо расписания).
type Filter struct {
	ID     string       `json:"id"`
	Chats  []int64      `json:"chats"`
	Match  Match        `json:"match"`
	Urgent bool         `json:"urgent"`
	Notify NotifyConfig `json:"notify"`
}

// FiltersConfig — обертка для корневого JSON: { "filters": [...] }.
// Удобно иметь явную структуру ради расширений и валидации верхнего уровня.
type FiltersConfig struct {
	Filters []Filter `json:"filters"`
}

// FilterEngine хранит загруженные фильтры и обеспечивает потокобезопасный доступ к ним.
type FilterEngine struct {
	filtersPath string
	filters     []Filter
	uniqueChats []int64
	mu          sync.RWMutex
}

func NewFilterEngine(filtersPath string) *FilterEngine {
	return &FilterEngine{
		filtersPath: filtersPath,
	}
}

// Load читает и парсит JSON-файл с фильтрами, обновляя внутреннее состояние.
func (fe *FilterEngine) Load() error {
	data, readErr := os.ReadFile(filepath.Clean(fe.filtersPath))
	if readErr != nil {
		return fmt.Errorf("failed to read filters json: %w", readErr)
	}

	var filtersConfig FiltersConfig
	if err := json.Unmarshal(data, &filtersConfig); err != nil {
		return fmt.Errorf("failed to unmarshal filters json: %w", err)
	}

	// Собираем уникальные чаты для быстрой проверки доступа/привязки диалогов
	unique := make(map[int64]struct{})
	for _, f := range filtersConfig.Filters {
		for _, chat := range f.Chats {
			unique[chat] = struct{}{}
		}
	}
	var chats []int64
	for chat := range unique {
		chats = append(chats, chat)
	}

	fe.mu.Lock()
	defer fe.mu.Unlock()
	fe.filters = filtersConfig.Filters
	fe.uniqueChats = chats

	return nil
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
