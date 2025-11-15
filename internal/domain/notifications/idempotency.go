// Package notifications: идемпотентность отправки через детерминированные random_id.
// Telegram дедуплицирует сообщения по random_id в пределах peer. Этот файл генерирует
// устойчивые random_id для текстов и пересылок, учитывая jobID, получателя, источник и позицию.
// Цель — исключить дубли при ретраях/перезапусках, сохраняя простоту вычисления.
package notifications

import (
	"encoding/binary"
	"hash/fnv"

	"telegram-userbot/internal/domain/filters"
)

const (
	// randomIDMask ограничивает значение до int63.
	// Требование Telegram: random_id ∈ [1, 2^63-1], 0 недопустим.
	// Маска и последующая проверка zero-защиты дают корректный диапазон.
	randomIDMask = (1 << 63) - 1 //nolint: mnd // так надо
)

// RandomIDForMessage генерирует детерминированный random_id для одиночного текстового отправления.
// Вход включает jobID и тип/ID получателя.
// Это гарантирует устойчивость к ретраям и отсутствие дублей в пределах адресата.
// Важно: уникальность обеспечивается на уровне комбинации (jobID, recipient).
func RandomIDForMessage(job Job, recipient filters.Recipient) int64 {
	return randomIDFromParts(
		uint64(job.ID),                   // #nosec G115
		uint64(job.CreatedAt.UnixNano()), // #nosec G115
		recipientTypeKey(recipient.Type.String()),
		uint64(recipient.PeerID), // #nosec G115
	)
}

// RandomIDsForForward генерирует вектор random_id для батч-пересылки исходных сообщений.
// Хэш смешивает jobID, тип/ID получателя, тип/ID источника и сами message_id.
// Дополнительно учитывается позиция i, чтобы различать сообщения с одинаковыми ID в разных позициях.
// Таким образом Telegram будет дедуплицировать ретраи, но не склеивать разные элементы батча.
func RandomIDsForForward(job Job, recipient filters.Recipient, from filters.Recipient, messageIDs []int) []int64 {
	out := make([]int64, len(messageIDs))
	for i, messageID := range messageIDs {
		out[i] = randomIDFromParts(
			uint64(job.ID),                   // #nosec G115
			uint64(job.CreatedAt.UnixNano()), // #nosec G115
			recipientTypeKey(recipient.Type.String()),
			uint64(recipient.PeerID),                 // #nosec G115
			recipientTypeKey(from.Type.String())+100, //nolint: mnd // так надо
			uint64(from.PeerID),                      // #nosec G115
			uint64(messageID),                        // #nosec G115
			uint64(i),                                // #nosec G115
		)
	}
	return out
}

// randomIDFromParts хэширует заданные части с помощью FNV-1a (64-бит) и проецирует в [1, 2^63-1].
// Используем LittleEndian для стабильного байтового представления. Ноль заменяется на 1.
func randomIDFromParts(parts ...uint64) int64 {
	// FNV-1a: быстрый и детерминированный. Коллизии теоретически возможны, на практике приемлемы для random_id.
	hasher := fnv.New64a()
	// Пакуем каждый uint64 в 8 байт и кормим хэшу по частям.
	var buf [8]byte
	for _, part := range parts {
		binary.LittleEndian.PutUint64(buf[:], part)
		_, _ = hasher.Write(buf[:])
	}
	// Приводим к допустимому диапазону Telegram и страхуемся от нуля ниже.
	value := hasher.Sum64() & randomIDMask
	if value == 0 {
		value = 1
	}
	return int64(value) // #nosec G115
}

// recipientTypeKey отображает строковый тип получателя в компактный числовой ключ для хэш-вклада.
// Значения фиксированы: user=1, chat=2, channel=3; 0 — для неизвестного.
func recipientTypeKey(t string) uint64 {
	switch t {
	case string(filters.RecipientTypeUser):
		// Значение 1 соответствует пользователям. Используем небольшие натуральные числа,
		// чтобы различия типов влияли на итоговый hash, но не требовали дополнительной памяти.
		return 1
	case string(filters.RecipientTypeChat):
		// Значение 2 выбрано для групповых чатов. Главное — уникальность относительно user/channel.
		return 2 //nolint: mnd // так надо
	case string(filters.RecipientTypeChannel):
		// Значение 3 назначено каналам и мегагруппам. Этого хватает, чтобы тип peer участвовал в hash.
		return 3 //nolint: mnd // так надо
	default:
		// Неизвестный тип приводит к нулю, что эквивалентно отсутствию дополнительного вклада.
		return 0
	}
}
