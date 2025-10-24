// Package core: файловое хранилище состояния апдейтов для gotd.
// Этот файл реализует StateStorage поверх JSON‑файла с ленивой загрузкой, потокобезопасным
// доступом (mutex) и атомарной записью на диск. Назначение — переживать рестарты userbot’а,
// сохраняя прогресс (Pts/Seq/Qts/Date и Pts по каналам) без потери событий.

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/storage"

	"github.com/go-faster/errors"
	"github.com/gotd/td/telegram/updates"
)

// fileStorage — потокобезопасное файловое хранилище состояний.
//   - path — путь к JSON‑файлу.
//   - loaded — признак ленивой инициализации из файла (истина после первой load()).
//   - states — мапа userID → updates.State (общие счетчики Pts/Seq/Qts/Date).
//   - channels — мапа userID → (channelID → Pts) для независимых каналов.
//
// Инварианты:
//  1. При SetState(u, s) всегда сбрасывается channels[u] в пустую мапу.
//  2. Все публичные методы вызывают load() под блокировкой mux.
type fileStorage struct {
	path string

	mux      sync.Mutex
	loaded   bool
	states   map[int64]updates.State
	channels map[int64]map[int64]int
}

// persisted — сериализуемая схема JSON‑файла. Ключи полей стабилизированы, чтобы
// возможные миграции были обратимы и предсказуемы.
type persisted struct {
	States   map[int64]updates.State `json:"states"`
	Channels map[int64]map[int64]int `json:"channels"`
}

// NewFileStorage создаёт хранилище с пустыми мапами и отложенной загрузкой с диска.
// Конструктор не трогает файловую систему: читаем/создаём файл при первом обращении (load()).
func NewFileStorage(path string) updates.StateStorage {
	return &fileStorage{
		path:     path,
		states:   map[int64]updates.State{},
		channels: map[int64]map[int64]int{},
	}
}

// ensureStateJSON гарантирует, что по указанному пути есть корректный JSON со схемой persisted.
// Делает:
//   - нормализует путь и создаёт каталог при необходимости,
//   - если файла нет или он пуст — пишет дефолтную структуру,
//   - если JSON битый — логирует предупреждение и переписывает дефолт,
//   - нормализует nil‑мапы и, при необходимости, сохраняет «исправленный» JSON.
//
// Возвращает раскодированную структуру.
func ensureStateJSON(path string) (persisted, error) {
	// Нормализуем путь и убеждаемся, что директория существует.
	clean := filepath.Clean(path)
	if err := storage.EnsureDir(clean); err != nil {
		return persisted{}, err
	}

	// Пробуем прочитать текущий файл состояния.
	bytes, err := os.ReadFile(clean)
	if os.IsNotExist(err) || len(bytes) == 0 {
		// Файл отсутствует или пуст — инициализируем дефолтной структурой и пишем атомарно.
		p := persisted{States: map[int64]updates.State{}, Channels: map[int64]map[int64]int{}}
		enc, mErr := json.MarshalIndent(p, "", "  ")
		if mErr != nil {
			return persisted{}, fmt.Errorf("encode default state: %w", mErr)
		}
		if wErr := storage.AtomicWriteFile(clean, enc); wErr != nil {
			return persisted{}, fmt.Errorf("init state file: %w", wErr)
		}
		logger.Debugf("StateStorage: created initial file %s", clean)
		return p, nil
	}
	if err != nil {
		return persisted{}, fmt.Errorf("read state: %w", err)
	}

	// Есть содержимое: пытаемся раскодировать JSON.
	var p persisted
	if uErr := json.Unmarshal(bytes, &p); uErr != nil {
		// Битый JSON — переписываем дефолтом и продолжаем с пустым состоянием.
		logger.Warnf("StateStorage: failed to decode %s: %v; rewriting default", clean, uErr)
		p = persisted{States: map[int64]updates.State{}, Channels: map[int64]map[int64]int{}}
		enc, mErr := json.MarshalIndent(p, "", "  ")
		if mErr != nil {
			return persisted{}, fmt.Errorf("encode default state: %w", mErr)
		}
		if wErr := storage.AtomicWriteFile(clean, enc); wErr != nil {
			return persisted{}, fmt.Errorf("rewrite default state: %w", wErr)
		}
		return p, nil
	}

	// Нормализуем возможные nil‑мапы, чтобы избежать паник при обращении.
	fixed := false
	if p.States == nil {
		p.States = make(map[int64]updates.State)
		fixed = true
	}
	if p.Channels == nil {
		p.Channels = make(map[int64]map[int64]int)
		fixed = true
	}
	if fixed {
		enc, mErr := json.MarshalIndent(p, "", "  ")
		if mErr != nil {
			return p, fmt.Errorf("encode fixed state: %w", mErr)
		}
		if wErr := storage.AtomicWriteFile(clean, enc); wErr != nil {
			return p, fmt.Errorf("persist fixed state: %w", wErr)
		}
	}
	return p, nil
}

// load выполняет ленивую загрузку состояния из файла. Предполагается, что вызывается под mux.
func (f *fileStorage) load() error {
	if f.loaded {
		return nil
	}
	p, err := ensureStateJSON(f.path)
	if err != nil {
		return err
	}
	f.states = p.States
	f.channels = p.Channels
	f.loaded = true
	return nil
}

// persist сериализует текущее состояние и атомарно записывает его на диск.
// Используется storage.AtomicWriteFile, чтобы не оставлять битых файлов.
func (f *fileStorage) persist() error {
	enc, err := json.MarshalIndent(persisted{
		States:   f.states,
		Channels: f.channels,
	}, "", "  ")
	if err != nil {
		return err
	}
	return storage.AtomicWriteFile(f.path, enc)
}

// GetState возвращает сохранённое состояние пользователя и флаг его наличия.
// Ошибки загрузки/чтения файла прокидываются вызывающему коду.
func (f *fileStorage) GetState(ctx context.Context, userID int64) (updates.State, bool, error) {
	f.mux.Lock()
	defer f.mux.Unlock()
	if err := f.load(); err != nil {
		return updates.State{}, false, err
	}
	st, ok := f.states[userID]
	return st, ok, nil
}

// SetState записывает полное состояние пользователя и сбрасывает связанные канал‑счетчики.
// Это обеспечивает согласованность: Pts по каналам не переживают смену базового состояния.
func (f *fileStorage) SetState(ctx context.Context, userID int64, state updates.State) error {
	f.mux.Lock()
	defer f.mux.Unlock()
	if err := f.load(); err != nil {
		return err
	}
	f.states[userID] = state
	// Сбрасываем карту каналов для пользователя, т.к. базовый state изменился.
	f.channels[userID] = map[int64]int{}
	return f.persist()
}

// SetPts обновляет Pts в основном состоянии пользователя и сразу сохраняет файл.
// Требует, чтобы для userID уже существовал state, иначе вернёт ошибку.
func (f *fileStorage) SetPts(ctx context.Context, userID int64, pts int) error {
	f.mux.Lock()
	defer f.mux.Unlock()
	if err := f.load(); err != nil {
		return err
	}
	st, ok := f.states[userID]
	if !ok {
		return errors.New("internalState not found")
	}
	st.Pts = pts
	f.states[userID] = st
	return f.persist()
}

// SetQts обновляет Qts для пользователя. Ошибка, если state отсутствует.
func (f *fileStorage) SetQts(ctx context.Context, userID int64, qts int) error {
	f.mux.Lock()
	defer f.mux.Unlock()
	if err := f.load(); err != nil {
		return err
	}
	st, ok := f.states[userID]
	if !ok {
		return errors.New("internalState not found")
	}
	st.Qts = qts
	f.states[userID] = st
	return f.persist()
}

// SetDate обновляет Date в состоянии пользователя. Ошибка, если state отсутствует.
func (f *fileStorage) SetDate(ctx context.Context, userID int64, date int) error {
	f.mux.Lock()
	defer f.mux.Unlock()
	if err := f.load(); err != nil {
		return err
	}
	st, ok := f.states[userID]
	if !ok {
		return errors.New("internalState not found")
	}
	st.Date = date
	f.states[userID] = st
	return f.persist()
}

// SetSeq обновляет Seq и синхронизирует изменения на диск. Ошибка при отсутствии state.
func (f *fileStorage) SetSeq(ctx context.Context, userID int64, seq int) error {
	f.mux.Lock()
	defer f.mux.Unlock()
	if err := f.load(); err != nil {
		return err
	}
	st, ok := f.states[userID]
	if !ok {
		return errors.New("internalState not found")
	}
	st.Seq = seq
	f.states[userID] = st
	return f.persist()
}

// SetDateSeq атомарно меняет Date и Seq за один проход с записью на диск.
func (f *fileStorage) SetDateSeq(ctx context.Context, userID int64, date, seq int) error {
	f.mux.Lock()
	defer f.mux.Unlock()
	if err := f.load(); err != nil {
		return err
	}
	st, ok := f.states[userID]
	if !ok {
		return errors.New("internalState not found")
	}
	st.Date = date
	st.Seq = seq
	f.states[userID] = st
	return f.persist()
}

// SetChannelPts сохраняет Pts для указанного канала пользователя. Ошибка, если для userID
// ещё не создано базовое состояние (карта каналов отсутствует).
func (f *fileStorage) SetChannelPts(ctx context.Context, userID, channelID int64, pts int) error {
	f.mux.Lock()
	defer f.mux.Unlock()
	if err := f.load(); err != nil {
		return err
	}
	chans, ok := f.channels[userID]
	if !ok {
		return errors.New("user internalState does not exist")
	}
	chans[channelID] = pts
	return f.persist()
}

// GetChannelPts возвращает Pts канала и флаг наличия. Если базового состояния нет — возвращает ok=false.
func (f *fileStorage) GetChannelPts(ctx context.Context, userID, channelID int64) (int, bool, error) {
	f.mux.Lock()
	defer f.mux.Unlock()
	if err := f.load(); err != nil {
		return 0, false, err
	}
	chans, ok := f.channels[userID]
	if !ok {
		return 0, false, nil
	}
	pts, ok := chans[channelID]
	return pts, ok, nil
}

// ForEachChannels вызывает fn для каждой пары (channelID, Pts) пользователя. Ошибка, если карта каналов отсутствует.
func (f *fileStorage) ForEachChannels(
	ctx context.Context,
	userID int64,
	fn func(ctx context.Context, channelID int64, pts int) error,
) error {
	f.mux.Lock()
	defer f.mux.Unlock()
	if err := f.load(); err != nil {
		return err
	}
	chans, ok := f.channels[userID]
	if !ok {
		return errors.New("channels map does not exist")
	}
	for id, pts := range chans {
		if err := fn(ctx, id, pts); err != nil {
			return err
		}
	}
	return nil
}
