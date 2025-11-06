// filter.go содержит структуры и методы для загрузки, хранения и управления
// фильтрами сообщений.
package filters

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"telegram-userbot/internal/infra/logger"
)

// MatchRules описывает правила совпадения входящих сообщений. Движок фильтрации
// трактует условие как И/ИЛИ/регексп в зависимости от полей:
//   - KeywordsAny: достаточно совпадения по ЛЮБОМУ из слов
//   - KeywordsAll: требуется совпадение по ВСЕМ словам
//   - Regex: проверка произвольным регулярным выражением
//   - ExcludeKeywordsAny / ExcludeRegex: выколотые множества для отрицания
//
// Совпадение считается успехом, если выполняется хотя бы одно «положительное»
// условие и одновременно не срабатывает ни одно исключающее.
type MatchRules struct {
	KeywordsAny        []string `json:"keywords_any"`
	KeywordsAll        []string `json:"keywords_all"`
	Regex              []string `json:"regex"`
	ExcludeKeywordsAny []string `json:"exclude_any"`
	ExcludeRegex       []string `json:"exclude_regex"`
}

type Notify struct {
	Urgent     bool     `json:"urgent"`
	Forward    bool     `json:"forward"`
	Recipients []string `json:"recipients"`
	Template   string   `json:"template"`
}

type Filter struct {
	ID     string     `json:"id"`
	Chats  []int64    `json:"chats"`
	Match  MatchRules `json:"match_rules"`
	Notify Notify     `json:"notify"`
}

// FiltersConfig — обертка для корневого JSON: { "filters": [...] }.
type FiltersConfig struct {
	Filters []Filter `json:"filters"`
}

// LoadFilters читает, парсит JSON-файл с фильтрами и возвращает срез Filter.
func LoadFilters(filePath string) ([]Filter, error) {
	data, readErr := os.ReadFile(filepath.Clean(filePath))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read filters json: %w", readErr)
	}

	var filters FiltersConfig
	if err := json.Unmarshal(data, &filters); err != nil {
		return nil, fmt.Errorf("failed to unmarshal filters json: %w", err)
	}

	logger.Infof("Loaded %d filters from %s", len(filters.Filters), filePath)
	return filters.Filters, nil
}
