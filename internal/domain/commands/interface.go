// Package commands предоставляет общий интерфейс для выполнения команд управления
// юзерботом. Команды используются как CLI-адаптером, так и веб-интерфейсом.
package commands

import (
	"context"
	"time"
)

// Executor - интерфейс для выполнения команд управления юзерботом.
type Executor interface {
	// Status возвращает текущий статус очереди уведомлений
	Status(ctx context.Context) (*StatusResult, error)

	// List возвращает список кешированных диалогов
	List(ctx context.Context) (*ListResult, error)

	// Flush инициирует немедленный слив регулярной очереди уведомлений
	Flush(ctx context.Context) error

	// RefreshDialogs обновляет кеш диалогов из Telegram API
	RefreshDialogs(ctx context.Context) error

	// ReloadFilters перезагружает фильтры и получателей из конфигурационных файлов
	ReloadFilters(ctx context.Context) error

	// Test отправляет тестовое сообщение администратору для проверки связности
	Test(ctx context.Context) (*TestResult, error)

	// Whoami возвращает информацию о текущем аккаунте
	Whoami(ctx context.Context) (*WhoamiResult, error)

	// Version возвращает информацию о версии приложения
	Version(ctx context.Context) (*VersionResult, error)
}

// StatusResult - результат команды Status
type StatusResult struct {
	UrgentQueueSize    int            // размер срочной очереди
	RegularQueueSize   int            // размер регулярной очереди
	LastRegularDrainAt time.Time      // время последнего слива регулярной очереди
	LastFlushAt        time.Time      // время последней персистентности
	NextScheduleAt     time.Time      // время следующего планового тика
	Location           *time.Location // таймзона для отображения
}

// ListResult - результат команды List
type ListResult struct {
	Dialogs []Dialog // список диалогов
}

// Dialog - информация о диалоге
type Dialog struct {
	ID       int64  // ID диалога
	Kind     string // тип диалога (user, chat, channel, folder)
	Title    string // название/имя
	Username string // username (если есть)
	Type     string // подтип (для каналов: Channel, Supergroup, Channel-like)
}

// TestResult - результат команды Test
type TestResult struct {
	Success bool      // успешна ли отправка
	Message string    // сообщение о результате
	SentAt  time.Time // время отправки
}

// WhoamiResult - результат команды Whoami
type WhoamiResult struct {
	ID       int64  // ID пользователя
	FullName string // полное имя
	Username string // username
}

// VersionResult - результат команды Version
type VersionResult struct {
	Name    string // название приложения
	Version string // версия
}
