// Package notifications — типы и утилиты для подготовки «копии текста» исходного
// сообщения к безопасной пересылке ботом. В рамках файла copy.go реализуется
// нормализация Telegram-entities в формат, совместимый с Bot API:
//   - сохраняется исходный текст без модификаций;
//   - индексы и длины сущностей — в UTF-16 кодовых единицах, как требует Bot API;
//   - медиа-части не поддерживаются осознанно — только текст + entities;
//   - предусмотрены деградации для типов, которых нет в Bot API.
package notifications

import (
	"fmt"

	"github.com/gotd/td/tg"
)

// CopyEntity описывает одну сущность форматирования для Bot API.
// Offset/Length заданы в UTF-16 кодовых единицах. Поля URL/Language/EmojiID
// заполняются только для соответствующих типов (text_link, pre, custom_emoji).
type CopyEntity struct {
	Type     string `json:"type"`                      // bold|italic|underline|strikethrough|code|pre|text_link|url|mention|hashtag|bot_command|email|phone_number|cash_tag|spoiler|custom_emoji
	Offset   int    `json:"offset"`                    // UTF-16
	Length   int    `json:"length"`                    // UTF-16
	URL      string `json:"url,omitempty"`             // для text_link
	Language string `json:"language,omitempty"`        // для pre
	EmojiID  string `json:"custom_emoji_id,omitempty"` // для custom_emoji (если доступен)
}

// CopyText — «готовая копия» текста исходного сообщения с нормализованными entities.
// Гарантии:
//   - Text совпадает с m.Message без изменений;
//   - Entities формируются в порядке, заданном телеграм-сервером;
//   - неизвестные/несовместимые сущности опускаются, текст не трогаем.
type CopyText struct {
	Text     string       `json:"text"`
	Entities []CopyEntity `json:"entities,omitempty"`
}

// BuildCopyTextFromTG формирует CopyText из tg.Message без изменения текста и индексов.
// Особенности и деградации:
//   - MessageEntityMentionName (text_mention) конвертируется в text_link с tg://user?id=<id>;
//   - MessageEntityCustomEmoji пропускается (EmojiID недоступен на этом этапе);
//   - неизвестные/будущие типы entities игнорируются без ошибки;
//   - offsets/length, приходящие от Telegram, считаются валидными и не
//     перерассчитываются в связи с суррогатными парами.
func BuildCopyTextFromTG(m *tg.Message) *CopyText {
	if m == nil || m.Message == "" {
		return &CopyText{Text: ""}
	}
	out := &CopyText{
		Text:     m.Message,
		Entities: make([]CopyEntity, 0, len(m.Entities)),
	}
	// Итерируемся по сущностям без индекса. Offsets/length прислал сервер и
	// они уже соответствуют UTF-16; перерасчёт не требуется.
	for _, ent := range m.Entities {
		switch v := ent.(type) {
		case *tg.MessageEntityBold:
			out.Entities = append(out.Entities, CopyEntity{Type: "bold", Offset: v.Offset, Length: v.Length})
		case *tg.MessageEntityItalic:
			out.Entities = append(out.Entities, CopyEntity{Type: "italic", Offset: v.Offset, Length: v.Length})
		case *tg.MessageEntityUnderline:
			out.Entities = append(out.Entities, CopyEntity{Type: "underline", Offset: v.Offset, Length: v.Length})
		case *tg.MessageEntityStrike:
			out.Entities = append(out.Entities, CopyEntity{Type: "strikethrough", Offset: v.Offset, Length: v.Length})
		case *tg.MessageEntityCode:
			out.Entities = append(out.Entities, CopyEntity{Type: "code", Offset: v.Offset, Length: v.Length})
		case *tg.MessageEntityPre:
			out.Entities = append(out.Entities, CopyEntity{Type: "pre", Offset: v.Offset, Length: v.Length, Language: v.Language})
		case *tg.MessageEntityURL:
			out.Entities = append(out.Entities, CopyEntity{Type: "url", Offset: v.Offset, Length: v.Length})
		case *tg.MessageEntityTextURL:
			out.Entities = append(out.Entities, CopyEntity{Type: "text_link", Offset: v.Offset, Length: v.Length, URL: v.URL})
		case *tg.MessageEntityMention:
			out.Entities = append(out.Entities, CopyEntity{Type: "mention", Offset: v.Offset, Length: v.Length})
		case *tg.MessageEntityHashtag:
			out.Entities = append(out.Entities, CopyEntity{Type: "hashtag", Offset: v.Offset, Length: v.Length})
		case *tg.MessageEntityBotCommand:
			out.Entities = append(out.Entities, CopyEntity{Type: "bot_command", Offset: v.Offset, Length: v.Length})
		case *tg.MessageEntityEmail:
			out.Entities = append(out.Entities, CopyEntity{Type: "email", Offset: v.Offset, Length: v.Length})
		case *tg.MessageEntityPhone:
			out.Entities = append(out.Entities, CopyEntity{Type: "phone_number", Offset: v.Offset, Length: v.Length})
		case *tg.MessageEntityCashtag:
			out.Entities = append(out.Entities, CopyEntity{Type: "cash_tag", Offset: v.Offset, Length: v.Length})
		case *tg.MessageEntitySpoiler:
			out.Entities = append(out.Entities, CopyEntity{Type: "spoiler", Offset: v.Offset, Length: v.Length})
		case *tg.MessageEntityMentionName: // text_mention(UserID)
			out.Entities = append(out.Entities, CopyEntity{
				Type: "text_link", Offset: v.Offset, Length: v.Length,
				URL: fmt.Sprintf("tg://user?id=%d", v.UserID),
			})
		case *tg.MessageEntityCustomEmoji:
			// На этом этапе нет надёжного способа получить custom_emoji_id для Bot API.
			// Без маппинга DocumentID -> EmojiID выполняем безопасную деградацию:
			// опускаем entity, оставляя текст нетронутым.
		default:
			// Неизвестные или не поддерживаемые Bot API типы игнорируем без изменения текста.
		}
	}
	return out
}
