// Package filters — сопоставление входящих сообщений с пользовательскими правилами.
//
// Назначение:
//   Этот пакет инкапсулирует бизнес‑логику матчинга сообщений по конфигурационным
//   фильтрам. На вход подаётся текст сообщения и набор правил из config.Filter,
//   на выходе — детализированное объяснение, какой фильтр сработал и почему.
//
// Модель данных и инварианты:
//   - Сопоставление нечувствительно к регистру (кроме regex).
//   - Для "словесных" ключей учитываются границы слов.
//   - Порядок правил в конфигурации важен: результаты возвращаются в том же порядке.
//   - Фильтр учитывается только для тех получателей, которые перечислены в Filter.Chats.
//
// Конвейер проверки одного фильтра (логическое И между include‑условиями и логическое НЕ для exclude):
//   1) includeRegex        — массив положительных regexp (логика ИЛИ). Пустой шаблон считается успешно пройденным;
//      используется re.FindString, то есть ищется непустая подстрока, а не полное совпадение.
//   2) includeKeywordsAll  — все слова из списка обязаны встретиться;
//   3) includeKeywordsAny  — достаточно хотя бы одного слова;
//   4) excludeByKeywords   — если встретилось любое "запрещённое" слово, фильтр отклоняется;
//   5) excludeByRegex      — массив запретительных regexp совпал (логика ИЛИ), фильтр отклоняется.
//
// Особенности и ограничения:
//   - Границы слов реализованы через Unicode-классы в регэкспе: (^|[^\p{L}\p{N}]) ... ([^\p{L}\p{N}]|$).
//     Это снимает проблему с (многобайтными) рунaми UTF-8 и даёт корректные границы для кириллицы и др.
//   - Ошибки компиляции регулярных выражений приводят к отклонению фильтра с логированием.
//   - Время работы линейное относительно длины текста и числа ключей,
//     если не учитывать сложность регулярных выражений.

package filters

import (
	"regexp"
	"slices"
	"strings"

	"telegram-userbot/internal/domain/tgutil"
	"telegram-userbot/internal/infra/logger"

	"github.com/gotd/td/tg"
)

// Result — детальный исход матчинга по одному фильтру.
// Поля:
//   - Matched    — финальный флаг срабатывания фильтра;
//   - Keywords   — список ключевых слов, которые были обнаружены. Порядок соответствует
//     порядку проверок: сначала KeywordsAll, затем KeywordsAny;
//   - RegexMatch — фрагмент текста, совпавший с положительным regexp (если он был задан).
type Result struct {
	Matched    bool
	Keywords   []string
	RegexMatch string
}

// FilterMatchResult связывает фильтр из конфигурации и его результат.
// Используется для логирования и дальнейшей бизнес‑обработки
// (например, выбор действия согласно типу фильтра).
type FilterMatchResult struct {
	Filter Filter
	Result Result
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
			results = append(results, FilterMatchResult{
				Filter: f,
				Result: res,
			})
		}
	}
	return results
}

// MatchMessage проверяет строку по одному фильтру, двигаясь по конвейеру:
//  1. includeRegex: пустой шаблон трактуется как "пройдено"; непустой должен дать совпадение
//     (ищется подстрока, а не полное совпадение).
//  2. includeKeywordsAll: если список пуст — пройдено; иначе все слова обязаны встретиться.
//  3. includeKeywordsAny: если список пуст — пройдено; иначе требуется как минимум одно слово.
//  4. excludeByKeywords: если встретилось любое слово из запрета — отклоняем.
//  5. excludeByRegex: если regex совпал — отклоняем.
//
// Поведение при ошибках:
//   - ошибки компиляции regexp приводят к возврату пустого Result и логируются;
//   - возвращаемый Result.Matched выставляется только в случае успешного прохождения всех стадий;
//   - Result.Keywords содержит объединённый список совпавших ключей из All и Any без дубликатов по позиции.
func MatchMessage(text string, f Filter) Result {
	result := Result{}

	if matched, ok, err := includeRegex(text, f.Match.Regex); ok {
		result.RegexMatch = matched
	} else {
		if err != nil {
			logger.Errorf("error in MatchMessage, includeRegex: %v", err)
		}
		return Result{}
	}

	if matched, ok := includeKeywordsAll(text, f.Match.KeywordsAll); ok {
		result.Keywords = append(result.Keywords, matched...)
	} else {
		return Result{}
	}

	if matched, ok := includeKeywordsAny(text, f.Match.KeywordsAny); ok {
		result.Keywords = append(result.Keywords, matched...)
	} else {
		return Result{}
	}

	if excludeByKeywords(text, f.Match.ExcludeKeywordsAny) {
		return Result{}
	}

	if ok, err := excludeByRegex(text, f.Match.ExcludeRegex); ok {
		return Result{}
	} else if err != nil {
		logger.Errorf("error in MatchMessage, excludeByRegex: %v", err)
		return Result{}
	}

	result.Keywords = dedupPreserveOrderCI(result.Keywords)
	result.Matched = true
	return result
}

// dedupPreserveOrderCI удаляет дубликаты из списка строк без учёта регистра,
func dedupPreserveOrderCI(ss []string) []string {
	if len(ss) > 1 {
		seen := make(map[string]struct{}, len(ss))
		out := ss[:0]
		for _, s := range ss {
			k := strings.ToLower(s)
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, s)
		}
		return out
	} else {
		return ss
	}
}

// includeRegex компилирует и применяет положительный regexp.
// Пустой pattern трактуется как "совпадает всегда".
// Возвращает найденный фрагмент, флаг ok и ошибку компиляции/применения.
// NB: используется re.FindString, поэтому совпадение ищется как подстрока.
func includeRegex(text string, pattern []string) (string, bool, error) {
	if len(pattern) == 0 {
		return "", true, nil
	}

	for _, p := range pattern {
		if p == "" {
			return "", true, nil
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return "", false, err
		}

		match := re.FindString(text)
		if match != "" {
			return match, true, nil
		}
	}

	return "", false, nil
}

// includeKeywordsAll — все ключевые слова должны присутствовать в тексте (без учёта регистра).
// Пустой список означает "условие выполнено".
// Возвращает список тех ключей, которые были найдены.
// Пример: text="foo bar", keywords={"foo","bar"} → ok=true, matched={"foo","bar"}.
func includeKeywordsAll(text string, keywords []string) ([]string, bool) {
	if len(keywords) == 0 {
		return nil, true
	}

	matched := make([]string, 0, len(keywords))
	for _, kw := range keywords {
		if !ContainsSmart(text, kw) {
			return nil, false
		}
		matched = append(matched, kw)
	}

	return matched, true
}

// includeKeywordsAny — достаточно хотя бы одного ключа (без учёта регистра).
// Пустой список — условие выполнено.
// Возвращает список совпавших ключей в порядке перечисления.
// Пример: text="foo baz", keywords={"bar","foo"} → ok=true, matched={"foo"}.
func includeKeywordsAny(text string, keywords []string) ([]string, bool) {
	if len(keywords) == 0 {
		return nil, true
	}

	matched := make([]string, 0, len(keywords))
	for _, kw := range keywords {
		if ContainsSmart(text, kw) {
			matched = append(matched, kw)
		}
	}

	if len(matched) == 0 {
		return nil, false
	}

	return matched, true
}

// excludeByKeywords — запретительный список: если встретился любой ключ, фильтр должен провалиться.
func excludeByKeywords(text string, keywords []string) bool {
	if len(keywords) == 0 {
		return false
	}

	for _, kw := range keywords {
		if ContainsSmart(text, kw) {
			return true
		}
	}

	return false
}

// excludeByRegex — запретительный regexp.
// Пустой шаблон означает "ничего не запрещено".
// При ошибке компиляции возвращается ошибка, чтобы вызывающий мог залогировать и отклонить фильтр.
func excludeByRegex(text string, pattern []string) (bool, error) {
	if len(pattern) == 0 {
		return false, nil
	}

	if matched, ok, err := includeRegex(text, pattern); ok && matched != "" {
		return true, nil
	} else if err != nil {
		return false, err
	}

	return false, nil
}

// ContainsSmart — проверка наличия ключа в тексте с учётом границ слов и регистра.
// Реализация: строится регэксп вида (?i)(^|[^\p{L}\p{N}])<kw>([^\p{L}\p{N}]|$), где <kw>
// экранирован через regexp.QuoteMeta. Это даёт:
//   - нечувствительность к регистру для Unicode;
//   - корректные границы слов для любых алфавитов (кириллица и др.);
//   - одинаковую логику как для "словесных" ключей, так и для ключей со спецсимволами.
//
// Примеры:
//
//	ContainsSmart("foo-bar", "foo")        == true
//	ContainsSmart("foobar", "foo")         == false
//	ContainsSmart("Привет, мир", "привет") == true
//	ContainsSmart("C++ guide", "C++")      == true
func ContainsSmart(text, kw string) bool {
	if kw == "" {
		return false
	}
	// Регэксп с Unicode-границами слов.
	// (?i) — регистронезависимость; QuoteMeta экранирует спецсимволы в ключе.
	pattern := `(?i)(^|[^\p{L}\p{N}])` + regexp.QuoteMeta(kw) + `([^\p{L}\p{N}]|$)`
	re := regexp.MustCompile(pattern)
	return re.FindStringIndex(text) != nil
}
