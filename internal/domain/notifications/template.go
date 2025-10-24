// Package notifications — подготовка текстов уведомлений и построение публичных ссылок.
// В этом файле собраны помощники для подстановки данных фильтров в шаблон
// и формирования t.me‑ссылок на сообщения, с приоритетом на entities, затем — кэш пиров.
// Бизнес‑назначение: сделать уведомления читабельными без внешних зависимостей от рендеринга.

package notifications

import (
	"fmt"
	"strings"
	"telegram-userbot/internal/domain/filters"
	"telegram-userbot/internal/infra/telegram/cache"

	"github.com/gotd/td/tg"
)

// RenderTemplate заполняет плоский текстовый шаблон плейсхолдерами:
//   - {{keywords}} — список сработавших ключевых слов, через запятую;
//   - {{regex}} — фрагмент, совпавший по положительному regex;
//   - {{message_link}} — публичная ссылка на сообщение.
//
// Если данных нет, {{keywords}} и {{regex}} очищаются до пустой строки, а {{message_link}} заменяется на «-».
// Никакого экранирования не выполняется: ожидается, что текст уже безопасен для выбранного транспорта.
func RenderTemplate(tmpl string, match filters.Result, link string) string {
	result := tmpl // рабочая копия шаблона, в которую последовательно вносятся замены
	// Замены выполняются буквально через strings.ReplaceAll, без шаблонизатора и без экранирования.
	if len(match.Keywords) > 0 {
		result = strings.ReplaceAll(result, "{{keywords}}", strings.Join(match.Keywords, ", "))
	} else {
		result = strings.ReplaceAll(result, "{{keywords}}", "")
	}
	if match.RegexMatch != "" {
		result = strings.ReplaceAll(result, "{{regex}}", match.RegexMatch)
	} else {
		result = strings.ReplaceAll(result, "{{regex}}", "")
	}
	if link != "" {
		result = strings.ReplaceAll(result, "{{message_link}}", link)
	} else {
		result = strings.ReplaceAll(result, "{{message_link}}", "-")
	}
	return result
}

// BuildMessageLink строит публичный URL для сообщения, если это возможно.
// Приоритет источников имени: сначала entities из апдейта, затем локальный кэш пиров.
// Поведение по типам peer:
//   - Channel: t.me/<username>/<msgID> при наличии username; иначе t.me/c/<channelID>/<msgID>.
//   - User: t.me/<username> (ссылка на профиль; прямой URL на конкретное сообщение у пользователей недоступен).
//   - Иное (PeerChat, приватные без username, юзеры без username): возвращается пустая строка.
func BuildMessageLink(entities tg.Entities, msg *tg.Message) string {
	switch peer := msg.PeerID.(type) {
	case *tg.PeerChannel:
		// Сначала пробуем достать username из свежих entities.
		if username := channelUsernameFromEntities(entities, peer.ChannelID); username != "" {
			return fmt.Sprintf("https://t.me/%s/%d", username, msg.ID)
		}
		// Затем — из локального кэша пиров.
		if username, ok := cache.ChannelUsername(peer.ChannelID); ok && username != "" {
			return fmt.Sprintf("https://t.me/%s/%d", username, msg.ID)
		}
		// Фолбэк для приватных каналов/супергрупп: числовая ссылка формата t.me/c/ID/msg.
		return fmt.Sprintf("https://t.me/c/%d/%d", peer.ChannelID, msg.ID)
	case *tg.PeerUser:
		// Для пользователей можно сослаться только на профиль, не на конкретное сообщение.
		if username := userUsernameFromEntities(entities, peer.UserID); username != "" {
			return fmt.Sprintf("https://t.me/%s", username)
		}
		if username, ok := cache.UserUsername(peer.UserID); ok && username != "" {
			return fmt.Sprintf("https://t.me/%s", username)
		}
	}

	// Всё остальное (PeerChat, приватные без username): публичной ссылки нет.
	return ""
}

// channelUsernameFromEntities возвращает username канала по идентификатору из
// набора entities. Если данные для указанного канала отсутствуют или пустые,
// возвращается пустая строка. Символ '@' обрезается, чтобы можно было формировать
// URL-адреса.
func channelUsernameFromEntities(entities tg.Entities, id int64) string {
	if ch, ok := entities.Channels[id]; ok && ch != nil {
		return strings.TrimPrefix(ch.Username, "@")
	}
	return ""
}

// userUsernameFromEntities извлекает username пользователя из entities по его
// идентификатору. Если пользователь не найден или username не задан, функция
// вернёт пустую строку. Символ '@' удаляется по тем же причинам, что и для
// каналов.
func userUsernameFromEntities(entities tg.Entities, id int64) string {
	if user, ok := entities.Users[id]; ok && user != nil {
		return strings.TrimPrefix(user.Username, "@")
	}
	return ""
}
