package filters

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"telegram-userbot/internal/domain/recipients"
	"telegram-userbot/internal/infra/logger"
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
	Recipients []string `json:"recipients"`
	Forward    bool     `json:"forward"`
	Template   string   `json:"template"`
}

// Filter описывает законченное правило обработки:
//   - ID: стабильный идентификатор правила (для логов, отладки, трекинга),
//   - Chats: из каких источников брать сообщения,
//   - Match: критерии совпадения,
//   - Urgent: маркер «срочных» уведомлений (может влиять на канал доставки),
//   - Notify: параметры маршрутизации уведомлений.
//   - ResolvedRecipients: развернутые получатели (внутреннее состояние, не сериализуется)
//
// NB: «срочность» никак не интерпретируется здесь в конфиге; семантика — на стороне
// потребителей (например, выбор немедленной отправки вместо расписания).
type Filter struct {
	ID     string       `json:"id"`
	Chats  []int64      `json:"chats"`
	Match  Match        `json:"match"`
	Urgent bool         `json:"urgent"`
	Notify NotifyConfig `json:"notify"`
	// НОВОЕ: развернутые получатели
	ResolvedRecipients []recipients.ResolvedRecipient `json:"-"`
}

// FiltersConfig — обертка для корневого JSON: { "filters": [...] }.
// Удобно иметь явную структуру ради расширений и валидации верхнего уровня.
type FiltersConfig struct {
	Filters []Filter `json:"filters"`
}

// FilterEngine хранит загруженные фильтры и обеспечивает потокобезопасный доступ к ним.
type FilterEngine struct {
	filtersPath    string
	recipientsPath string // НОВОЕ: путь к файлу получателей
	filters        []Filter
	uniqueChats    []int64
	recipients     *recipients.RecipientManager // НОВОЕ: внутренняя зависимость
	mu             sync.RWMutex
}

func NewFilterEngine(filtersPath string, recipientsPath string) *FilterEngine {
	return &FilterEngine{
		filtersPath:    filtersPath,
		recipientsPath: recipientsPath,
		recipients:     recipients.NewRecipientManager(recipientsPath),
	}
}

// Load читает и парсит JSON-файл с фильтрами и файл с получателями,
// обновляя внутреннее состояние.
func (fe *FilterEngine) Load() error {
	// Загружаем recipients через внутренний RecipientManager
	if err := fe.recipients.Load(); err != nil {
		return fmt.Errorf("failed to load recipients: %w", err)
	}

	// Читаем и парсим JSON-файл с фильтрами
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

	// Обрабатываем каждый фильтр: проверяем и разворачиваем получателей
	var validFilters []Filter
	for _, filter := range filtersConfig.Filters {
		if len(filter.Notify.Recipients) == 0 {
			// Пропускаем фильтр без получателей
			logger.Errorf("Filter '%s' has empty recipients list, skipping", filter.ID)
			continue
		} else if err := fe.recipients.ValidateIDs(filter.Notify.Recipients); err != nil {
			// Пропускаем фильтр с невалидными ID получателей
			logger.Errorf("Filter '%s' has invalid recipients: %v, skipping", filter.ID, err)
			continue
		}

		// Разворачиваем получателей из ID в полные структуры
		resolvedRecipients := fe.recipients.ResolveToTargets(filter.Notify.Recipients)
		if len(resolvedRecipients) == 0 {
			logger.Errorf("Filter '%s' has no resolved recipients, skipping", filter.ID)
			continue
		}

		// Создаем фильтр с развернутыми получателями
		validFilter := Filter{
			ID:                 filter.ID,
			Chats:              filter.Chats,
			Match:              filter.Match,
			Urgent:             filter.Urgent,
			Notify:             filter.Notify,
			ResolvedRecipients: resolvedRecipients,
		}

		// Фильтр валиден, добавляем его
		validFilters = append(validFilters, validFilter)
	}

	fe.mu.Lock()
	defer fe.mu.Unlock()
	fe.filters = validFilters
	fe.uniqueChats = chats

	return nil
}

// Reload перезагружает всё (recipients + filters) с rollback при ошибке.
func (fe *FilterEngine) Reload() error {
	// Сохраняем текущее состояние на случай отката
	fe.mu.RLock()
	oldFilters := make([]Filter, len(fe.filters))
	copy(oldFilters, fe.filters)
	oldUniqueChats := make([]int64, len(fe.uniqueChats))
	copy(oldUniqueChats, fe.uniqueChats)
	fe.mu.RUnlock()

	// Загружаем новые данные
	if err := fe.recipients.Load(); err != nil {
		return fmt.Errorf("failed to reload recipients: %w", err)
	}

	data, readErr := os.ReadFile(filepath.Clean(fe.filtersPath))
	if readErr != nil {
		return fmt.Errorf("failed to read filters json: %w", readErr)
	}

	var filtersConfig FiltersConfig
	if err := json.Unmarshal(data, &filtersConfig); err != nil {
		return fmt.Errorf("failed to unmarshal filters json: %w", err)
	}

	// Собираем уникальные чаты
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

	// Обрабатываем каждый фильтр: проверяем и разворачиваем получателей
	var validFilters []Filter
	for _, filter := range filtersConfig.Filters {
		if len(filter.Notify.Recipients) == 0 {
			// Пропускаем фильтр без получателей
			logger.Errorf("Filter '%s' has empty recipients list, skipping", filter.ID)
			continue
		} else if err := fe.recipients.ValidateIDs(filter.Notify.Recipients); err != nil {
			// Пропускаем фильтр с невалидными ID получателей
			logger.Errorf("Filter '%s' has invalid recipients: %v, skipping", filter.ID, err)
			continue
		}

		// Разворачиваем получателей из ID в полные структуры
		resolvedRecipients := fe.recipients.ResolveToTargets(filter.Notify.Recipients)
		if len(resolvedRecipients) == 0 {
			logger.Errorf("Filter '%s' has no resolved recipients, skipping", filter.ID)
			continue
		}

		// Создаем фильтр с развернутыми получателями
		validFilter := Filter{
			ID:                 filter.ID,
			Chats:              filter.Chats,
			Match:              filter.Match,
			Urgent:             filter.Urgent,
			Notify:             filter.Notify,
			ResolvedRecipients: resolvedRecipients,
		}

		// Фильтр валиден, добавляем его
		validFilters = append(validFilters, validFilter)
	}

	// Захватываем мьютекс и обновляем состояние
	fe.mu.Lock()
	defer fe.mu.Unlock()

	// Проверяем, что процесс загрузки завершился успешно
	if len(validFilters) == 0 {
		// Если нет валидных фильтров, откатываемся к старому состоянию
		logger.Error("Reload failed: no valid filters found, rolling back to previous state")
		fe.filters = oldFilters
		fe.uniqueChats = oldUniqueChats
		return fmt.Errorf("no valid filters found after reload")
	}

	// Обновляем состояние
	fe.filters = validFilters
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
