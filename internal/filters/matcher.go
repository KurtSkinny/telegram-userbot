// matcher.go содержит функции и логику обработки правил фильтрации сообщений.
package filters

import (
	"regexp"
	"strings"
)

// MatchMessage проверяет строку по одному фильтру с новой системой фильтрации.
// Использует детерминированную логику с DENY и ALLOW этапами:
// 1. DENY: жёсткая чистка мусора. Если совпало хоть одно правило deny — сообщение выбрасывается.
// 2. ALLOW: выборка нужного. Если есть правила allow, сообщение должно им соответствовать.
func MatchMessage(text string, f Filter) FilterResult {
	// Нормализуем текст для проверки
	normalizedText := normalizeText(text)

	// Проверяем DENY
	if f.Rules.Deny != nil {
		if match, matchedNode := evalNode(f.Rules.Deny, normalizedText); match {
			return FilterResult{
				Matched:     false,
				ResultType:  Drop,
				MatchedNode: *matchedNode,
			}
		}
	}

	// Проверяем ALLOW
	if f.Rules.Allow != nil {
		if match, matchedNode := evalNode(f.Rules.Allow, normalizedText); match {
			return FilterResult{
				Matched:     true,
				ResultType:  AllowMatch,
				MatchedNode: *matchedNode,
			}
		} else {
			return FilterResult{
				Matched:     false,
				ResultType:  NoMatch,
				MatchedNode: *matchedNode,
			}
		}
	}

	// Если allow отсутствует или пуст - PASS_THROUGH
	return FilterResult{
		Matched:     true,
		ResultType:  PassThrough,
		MatchedNode: Node{}, // No specific node matched
	}
}

// normalizeText нормализует текст для проверки:
// - ё->е
// - схлопываем пробелы
// - приводим любые пробельные символы к одному пробелу
func normalizeText(text string) string {
	// Заменяем ё на е
	result := strings.ReplaceAll(text, "ё", "е")
	result = strings.ReplaceAll(result, "Ё", "Е")

	// Заменяем любые пробельные символы одним пробелом и схлопываем повторы
	result = regexp.MustCompile(`\s+`).ReplaceAllString(result, " ")

	return strings.TrimSpace(result)
}
