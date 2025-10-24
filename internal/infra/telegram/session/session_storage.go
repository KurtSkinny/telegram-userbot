package session

// Пакет session содержит обёртки поверх tdsession.Storage для MTProto‑сессий.
// Цели:
//   - атомарная запись файла сессии на диск (без частичных состояний);
//   - сигнализация менеджеру соединения о готовности/восстановлении сессии;
//   - потокобезопасный доступ к файловой системе при конкурирующих вызовах.
// Обновление сессии обычно свидетельствует об успешном логине/реавторизации, поэтому
// после удачной записи уведомляем connection.Manager, чтобы разблокировать ожидателей.

import (
	"context"
	"fmt"
	"os"
	"sync"

	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/storage"
	"telegram-userbot/internal/infra/telegram/connection"

	tdsession "github.com/gotd/td/session"
)

// NotifyStorage реализует tdsession.Storage поверх обычного файла и дополняет
// сохранение уведомлением connection.Manager о том, что сессия актуальна.
// Потокобезопасен: операции Load/Store защищены мьютексом. Поле Path указывает
// абсолютный или относительный путь до файла сессии на диске.
type NotifyStorage struct {
	Path string
	mux  sync.Mutex
}

// Компиляторная проверка соответствия интерфейсу tdsession.Storage.
var _ tdsession.Storage = (*NotifyStorage)(nil)

// LoadSession читает файл сессии с диска под мьютексом. Возвращает
// tdsession.ErrNotFound, если файл отсутствует, что соответствует контракту
// хранилища сессии. Любые другие ошибки заворачиваются с контекстом.
func (n *NotifyStorage) LoadSession(_ context.Context) ([]byte, error) {
	n.mux.Lock()
	// Сериализуем доступ к файлу сессии, чтобы избежать гонок при одновременных вызовах.
	defer n.mux.Unlock()

	// Читаем содержимое файла полностью; отсутствие файла интерпретируем как «сессия не найдена».
	data, err := os.ReadFile(n.Path)
	if os.IsNotExist(err) {
		return nil, tdsession.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}
	return data, nil
}

// StoreSession атомарно сохраняет данные сессии на диск и уведомляет менеджер
// соединения о готовности. Запись выполняется через storage.AtomicWriteFile,
// поэтому либо файл полностью обновлён, либо остаётся предыдущая корректная версия.
func (n *NotifyStorage) StoreSession(_ context.Context, data []byte) error {
	n.mux.Lock()
	// Блокируем конкурентные записи, чтобы не получить частично записанные данные.
	defer n.mux.Unlock()

	// Атомарная запись: создаём временный файл и заменяем целевой, что устойчиво к сбоям.
	if err := storage.AtomicWriteFile(n.Path, data); err != nil {
		return fmt.Errorf("atomic write session: %w", err)
	}
	// Сигнализируем connection.Manager: сессия актуальна, можно разблокировать WaitOnline.
	logger.Debug("StoreSession: connection.MarkConnected")
	connection.MarkConnected()
	return nil
}
