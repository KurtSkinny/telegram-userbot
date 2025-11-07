// filter.go содержит структуры и методы для загрузки, хранения и управления
// фильтрами сообщений.
package filters

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"telegram-userbot/internal/infra/logger"
)

// Node представляет узел в дереве фильтрации.
type Node struct {
	Op      string `json:"op,omitempty"`      // AND, OR, NOT, AT_LEAST
	Type    string `json:"type,omitempty"`    // kw, re (for leaf nodes)
	Value   string `json:"value,omitempty"`   // значение для leaf узлов
	Pattern string `json:"pattern,omitempty"` // паттерн для регулярных выражений
	N       int    `json:"n,omitempty"`       // для AT_LEAST
	Args    []Node `json:"args,omitempty"`    // для логических узлов

	CompiledPattern *regexp.Regexp `json:"-"`
}

// FilterRule содержит правила фильтрации: deny и allow.
type FilterRule struct {
	Deny  *Node `json:"deny,omitempty"`
	Allow *Node `json:"allow,omitempty"`
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
	Rules  FilterRule `json:"rules"`
	Notify Notify     `json:"notify"`
}

// FiltersConfig — обертка для корневого JSON: { "filters": [...] }.
type FiltersConfig struct {
	Filters []Filter `json:"filters"`
}

// ValidateAndCompile валидирует структуру узла и компилирует все регулярные выражения.
// Рекурсивно обходит дерево и кеширует скомпилированные паттерны.
func (n *Node) ValidateAndCompile() error {
	if n == nil {
		return errors.New("node is nil")
	}

	// Leaf node
	if n.Op == "" {
		return n.compileLeafPattern()
	}

	switch n.Op {
	case "AND", "OR":
		return n.validateNaryOp()
	case "NOT":
		return n.validateUnaryOp()
	case "AT_LEAST":
		return n.validateAtLeast()
	default:
		return fmt.Errorf("unknown operator: %s", n.Op)
	}
}

func (n *Node) validateNaryOp() error {
	const minArgs = 2
	if len(n.Args) < minArgs {
		return fmt.Errorf("operator %s requires at least %d arguments, got %d", n.Op, minArgs, len(n.Args))
	}
	for i := range n.Args {
		if err := (&n.Args[i]).ValidateAndCompile(); err != nil {
			return fmt.Errorf("invalid argument %d for %s: %w", i, n.Op, err)
		}
	}
	return nil
}

func (n *Node) validateUnaryOp() error {
	if len(n.Args) != 1 {
		return fmt.Errorf("operator NOT requires exactly 1 argument, got %d", len(n.Args))
	}
	if err := (&n.Args[0]).ValidateAndCompile(); err != nil {
		return fmt.Errorf("invalid argument for NOT: %w", err)
	}
	return nil
}

func (n *Node) validateAtLeast() error {
	const minArgs = 2
	if len(n.Args) < minArgs {
		return fmt.Errorf("operator AT_LEAST requires at least %d arguments, got %d", minArgs, len(n.Args))
	}
	if n.N < 1 || n.N > len(n.Args) {
		return fmt.Errorf("AT_LEAST n=%d is invalid (must be 1 <= n <= %d)", n.N, len(n.Args))
	}
	for i := range n.Args {
		if err := (&n.Args[i]).ValidateAndCompile(); err != nil {
			return fmt.Errorf("invalid argument %d for AT_LEAST: %w", i, err)
		}
	}
	return nil
}

// compileLeafPattern компилирует паттерн для листового узла (kw или re).
func (n *Node) compileLeafPattern() error {
	switch n.Type {
	case "kw":
		if n.Value == "" {
			return errors.New("keyword value cannot be empty")
		}
		// Нормализуем ключевое слово: lowercase + схлопывание пробелов
		normalizedKw := regexp.MustCompile(`\s+`).ReplaceAllString(strings.ToLower(n.Value), " ")
		normalizedKw = strings.TrimSpace(normalizedKw)

		// Создаем regexp с Unicode-границами слов
		// (?i) — регистронезависимость
		pattern := `(?i)(^|[^\p{L}\p{N}_])` + regexp.QuoteMeta(normalizedKw) + `([^\p{L}\p{N}_]|$)`

		compiledRe, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("failed to compile keyword pattern for '%s': %w", n.Value, err)
		}
		n.CompiledPattern = compiledRe
		logger.Debugf("Compiled keyword pattern: '%s' -> %s", n.Value, pattern)
		return nil

	case "re":
		if n.Pattern == "" {
			return errors.New("regex pattern cannot be empty")
		}
		compiledRe, err := regexp.Compile(n.Pattern)
		if err != nil {
			return fmt.Errorf("failed to compile regex pattern '%s': %w", n.Pattern, err)
		}
		n.CompiledPattern = compiledRe
		logger.Debugf("Compiled regex pattern: %s", n.Pattern)
		return nil

	default:
		return fmt.Errorf("unknown node type: %s (expected 'kw' or 're')", n.Type)
	}
}

// ValidateFilter проверяет корректность фильтра.
func (f *Filter) ValidateFilter() error {
	if f.ID == "" {
		return errors.New("filter ID cannot be empty")
	}
	if len(f.Chats) == 0 {
		return fmt.Errorf("filter %s has no chats", f.ID)
	}

	// Проверяем, что есть хотя бы одно правило
	if f.Rules.Deny == nil && f.Rules.Allow == nil {
		return fmt.Errorf("filter %s has no deny or allow rules", f.ID)
	}

	// Валидируем и компилируем deny правило
	if f.Rules.Deny != nil {
		if err := f.Rules.Deny.ValidateAndCompile(); err != nil {
			return fmt.Errorf("filter %s has invalid deny rule: %w", f.ID, err)
		}
	}

	// Валидируем и компилируем allow правило
	if f.Rules.Allow != nil {
		if err := f.Rules.Allow.ValidateAndCompile(); err != nil {
			return fmt.Errorf("filter %s has invalid allow rule: %w", f.ID, err)
		}
	}

	// Проверяем notify секцию
	if len(f.Notify.Recipients) == 0 {
		logger.Warnf("filter %s has no recipients", f.ID)
	}

	return nil
}

// LoadFilters читает, парсит JSON-файл с фильтрами и возвращает срез Filter.
func LoadFilters(filePath string) ([]Filter, error) {
	data, readErr := os.ReadFile(filepath.Clean(filePath))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read filters json: %w", readErr)
	}

	var filtersConfig FiltersConfig
	if err := json.Unmarshal(data, &filtersConfig); err != nil {
		return nil, fmt.Errorf("failed to unmarshal filters json: %w", err)
	}

	// Валидация: id фильтров должны быть уникальными
	ids := make(map[string]bool)
	filters := make([]Filter, 0, len(filtersConfig.Filters))

	for _, f := range filtersConfig.Filters {
		if ids[f.ID] {
			return nil, fmt.Errorf("duplicate filter ID: %s", f.ID)
		}
		ids[f.ID] = true

		// Валидируем и компилируем фильтр
		if err := f.ValidateFilter(); err != nil {
			logger.Errorf("invalid filter %s: %v, skipping", f.ID, err)
			continue
		}

		filters = append(filters, f)
	}

	if len(filters) == 0 {
		return nil, fmt.Errorf("no valid filters loaded from %s", filePath)
	}

	logger.Infof("Successfully loaded %d filters from %s", len(filters), filePath)
	return filters, nil
}
