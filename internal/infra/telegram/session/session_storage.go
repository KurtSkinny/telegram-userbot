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

	"github.com/go-faster/errors"

	tdsession "github.com/gotd/td/session"
)

// FileStorage реализует tdsession.Storage поверх обычного файла и дополняет
// сохранение уведомлением connection.Manager о том, что сессия актуальна.
// Потокобезопасен: операции Load/Store защищены мьютексом. Поле Path указывает
// абсолютный или относительный путь до файла сессии на диске.
type FileStorage struct {
	Path string
	mux  sync.Mutex
}

// Компиляторная проверка соответствия интерфейсу tdsession.Storage.
var _ tdsession.Storage = (*FileStorage)(nil)

// LoadSession читает файл сессии с диска.
func (f *FileStorage) LoadSession(_ context.Context) ([]byte, error) {
	if f == nil {
		return nil, errors.New("nil session storage is invalid")
	}
	f.mux.Lock()
	defer f.mux.Unlock()

	data, err := os.ReadFile(f.Path)
	if os.IsNotExist(err) {
		return nil, tdsession.ErrNotFound
	}
	if err != nil {
		return nil, errors.Wrap(err, "read session")
	}
	return data, nil
}

// StoreSession атомарно сохраняет данные сессии на диск и уведомляет менеджер
// соединения о готовности.
func (f *FileStorage) StoreSession(_ context.Context, data []byte) error {
	if f == nil {
		return errors.New("nil session storage is invalid")
	}

	f.mux.Lock()
	defer f.mux.Unlock()

	if err := storage.AtomicWriteFile(f.Path, data); err != nil {
		return fmt.Errorf("atomic write session: %w", err)
	}

	// Сигнализируем connection.Manager: сессия актуальна, можно разблокировать WaitOnline.
	logger.Debug("StoreSession: connection.MarkConnected")
	connection.MarkConnected()
	return nil
}
