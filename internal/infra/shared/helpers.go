// Package shared — небольшие общие утилиты без внешних зависимостей.
// Содержит обобщённые функции для работы со слайсами и числовыми диапазонами.
// Фокус: безопасные операции без паник, сохранение порядка и простая семантика.
package shared

import "math/rand/v2"

// Unique возвращает срез уникальных значений, сохраняя порядок первого появления.
// Работает для любых типов с сравнимостью (comparable). Сложность O(n) по времени
// и O(n) по памяти на карту «виденных» значений. Порядок стабильный.
func Unique[T comparable](in []T) []T {
	seen := make(map[T]struct{}, len(in))
	out := make([]T, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// GetAt безопасно возвращает элемент слайса по индексу i. В случае выхода за
// границы возвращает нулевое значение типа T и false, без паники. Полезно как
// неболезненная альтернатива ручным проверкам длины перед обращением.
func GetAt[T any](s []T, i int) (T, bool) {
	if i < 0 || i >= len(s) {
		var zero T
		return zero, false
	}
	return s[i], true
}

// Random возвращает псевдослучайное целое в диапазоне [fromMin, toMax] включительно.
// Если fromMin >= toMax, возвращается fromMin. Используется math/rand/v2; криптостойкость
// не требуется, поэтому пометка #nosec G404 осознанна.
func Random(fromMin, toMax int) int {
	if fromMin >= toMax {
		return fromMin
	}
	// Смещение на +fromMin после IntN(toMax-fromMin+1) даёт включительный верхний предел.
	return rand.IntN(toMax-fromMin+1) + fromMin // #nosec G404
}
