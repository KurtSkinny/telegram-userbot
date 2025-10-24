// Package storage — утилиты безопасной работы с локальным хранилищем.
// В этом файле реализованы:
//   - EnsureDir — гарантирует наличие директории для целевого пути;
//   - AtomicWriteFile — атомарная запись файла с синхронизацией данных и метаданных.
//
// Используется для хранения MTProto-сессий и прочих чувствительных данных, где
// недопустимы частично записанные файлы.
package storage

import (
	"fmt"
	"os"
	"path/filepath"

	"telegram-userbot/internal/infra/logger"
)

// defaultFilePerm — права, выставляемые на итоговый файл при атомарной записи.
// Значение 0o600 ограничивает доступ только владельцу процесса.
const defaultFilePerm = 0600

// EnsureDir гарантирует наличие каталога для указанного файла.
// Если путь не содержит директорию ("." или пустая строка), ничего не делает.
// Создание выполняется с правами 0o700, ошибки оборачиваются с указанием каталога.
func EnsureDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "." || dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}
	return nil
}

// AtomicWriteFile атомарно записывает байты в файл path.
//
// Алгоритм: temp в той же директории → write → fsync(temp) → chmod(defaultFilePerm)
// → close → rename → fsync(dir). Это гарантирует, что либо старый файл остаётся
// цел, либо новый записан полностью. Важно: os.Rename атомарен только в пределах
// одного файлового тома. fsync каталога выполняется по принципу best‑effort и
// может игнорироваться некоторыми ОС/ФС, но заметно повышает надёжность метаданных.
// Права на итоговый файл задаются значением defaultFilePerm (0o600).
func AtomicWriteFile(path string, data []byte) error {
	// Нормализуем путь и работаем только с очищённым значением.
	clean := filepath.Clean(path)
	// Гарантируем существование каталога.
	if err := EnsureDir(clean); err != nil {
		return err
	}
	dir := filepath.Dir(clean)

	var tmp *os.File
	// Создаём temp в том же каталоге, чтобы rename был атомарным.
	if tmpFile, err := os.CreateTemp(dir, "atomic-*.tmp"); err != nil {
		return fmt.Errorf("create temp file: %w", err)
	} else {
		tmp = tmpFile
	}

	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	// Пишем данные.
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	// Синхронизируем содержимое temp на диск.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync temp file: %w", err)
	}
	// Выставляем права для будущего целевого файла.
	if err := tmp.Chmod(defaultFilePerm); err != nil {
		// Не критично, но лучше не скрывать проблему прав.
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	// Закрываем — теперь можно переименовывать.
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// Атомарная замена: на POSIX rename поверх существующего файла — атомарна.
	// Важно: path должен лежать на том же файловом томе, что и temp.
	if err := os.Rename(tmpName, clean); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	// fsync каталога повышает надёжность метаданных (журналирование записи имени файла).
	if dirFile, err := os.Open(dir); err == nil {
		if errSync := dirFile.Sync(); errSync != nil {
			logger.Warnf("AtomicWriteFile: dir sync error: %v", errSync) // best-effort для Windows/некоторых FS
		}
		_ = dirFile.Close()
	}
	return nil
}
