package recipients

import "encoding/json"

// RecipientTargets поддерживает два формата конфигурации получателей:
//  1. исторический плоский массив []int64 (трактуется как Users),
//  2. новый объект с раздельными полями users/chats/channels.
//
// Метод UnmarshalJSON обеспечивает обратную совместимость и устраняет
// нули/дубликаты, сохраняя порядок первых вхождений.
type RecipientTargets struct {
	Users    []int64 `json:"users"`
	Chats    []int64 `json:"chats"`
	Channels []int64 `json:"channels"`
}

// UnmarshalJSON реализует особую десериализацию для RecipientTargets:
// пытается распарсить вход как []int64; если не получилось — как объект
// {users, chats, channels}. Нули и повторы удаляются, чтобы избежать кривых
// адресатов в рантайме при доставке уведомлений.
func (r *RecipientTargets) UnmarshalJSON(b []byte) error {
	// Попытка распарсить как старый формат: []int64
	var flat []int64
	if err := json.Unmarshal(b, &flat); err == nil {
		r.Users = uniqueNonZero(flat)
		r.Chats = nil
		r.Channels = nil
		return nil
	}
	// Пробуем новый формат: объект
	var tmp struct {
		Users    []int64 `json:"users"`
		Chats    []int64 `json:"chats"`
		Channels []int64 `json:"channels"`
	}
	if err := json.Unmarshal(b, &tmp); err != nil {
		return err
	}
	r.Users = uniqueNonZero(tmp.Users)
	r.Chats = uniqueNonZero(tmp.Chats)
	r.Channels = uniqueNonZero(tmp.Channels)
	return nil
}

// uniqueNonZero удаляет нули и дубликаты из списка ID, сохраняя порядок
// первого появления. Возвращает nil, если в результате список пуст.
//
// Причина: нулевые/повторные ID часто возникают при ручном редактировании
// конфигов и приводят к «мусору» при маршрутизации уведомлений.
func uniqueNonZero(in []int64) []int64 {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if v == 0 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
