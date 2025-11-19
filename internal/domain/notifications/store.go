// Package notifications: файловые сторы очереди.
// Этот файл реализует QueueStore (persist с дебаунсом и flush/close протоколом)
// и FailedStore (журнал финальных неудач) поверх JSON с атомарной записью.
// Назначение: долговременная устойчивость очереди и восстановление после рестартов.
package notifications

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/storage"
)

// flushRequest используется воркером для синхронного завершения отложенной записи.
// Канал reply получает итог ошибки consumePending().
// type flushRequest struct {
// 	reply chan error
// }

// QueueStore — фоновый сервис персиста состояния очереди в JSON.
// Особенности:
//   - атомарная запись через временный файл,
//   - дебаунс, чтобы не молотить диск при бурстах,
//   - неблокирующий backpressure: в updates держится только последний снапшот,
//   - безопасное завершение: Flush/Close, сохранение первой ошибки (finalErr).
type QueueStore struct {
	path     string
	debounce time.Duration

	updates chan State
	stopCh  chan struct{}

	wg       sync.WaitGroup
	finalErr error
	errMu    sync.Mutex

	stopOnce  sync.Once
	startOnce sync.Once
}

// NewQueueStore подготавливает файловое хранилище: нормализует путь,
// гарантирует валидный JSON (ensureStateFile) и создаёт управляющие каналы.
// Запуска фона не делает; для обработки persist вызовите Start().
func NewQueueStore(path string, debounce time.Duration) (*QueueStore, error) {
	clean := filepath.Clean(path)
	if _, err := ensureStateFile(clean); err != nil {
		return nil, err
	}
	store := &QueueStore{
		path:     clean,
		debounce: debounce,
		updates:  make(chan State, 1),
		stopCh:   make(chan struct{}),
	}
	return store, nil
}

// ensureStateFile проверяет/создаёт файл состояния очереди.
// Поведение:
//   - если файла нет или он пуст — записывает DefaultState();
//   - если JSON битый — логирует предупреждение и перезаписывает DefaultState();
//   - нормализует инварианты: NextID>=1, non-nil срезы Regular/Urgent;
//   - все записи производятся атомарно через storage.AtomicWriteFile.
//
// Возвращает восстановленное состояние.
func ensureStateFile(path string) (State, error) {
	clean := filepath.Clean(path)

	bytes, errRead := os.ReadFile(clean)
	if os.IsNotExist(errRead) || len(bytes) == 0 {
		st := DefaultState()
		b, errJSON := json.MarshalIndent(st, "", "  ")
		if errJSON != nil {
			return DefaultState(), fmt.Errorf("encode default queue state: %w", errJSON)
		}
		if err := storage.AtomicWriteFile(clean, b); err != nil {
			return DefaultState(), fmt.Errorf("init queue state file: %w", err)
		}
		logger.Debugf("QueueStore: created initial state file %s", clean)
		return st, nil
	}
	if errRead != nil {
		return DefaultState(), fmt.Errorf("read queue state: %w", errRead)
	}

	var st State
	if errUnmarsh := json.Unmarshal(bytes, &st); errUnmarsh != nil {
		logger.Warnf("QueueStore: failed to decode %s: %v; rewriting default", clean, errUnmarsh)
		st = DefaultState()
		b, errJSON := json.MarshalIndent(st, "", "  ")
		if errJSON != nil {
			return DefaultState(), fmt.Errorf("encode default queue state: %w", errJSON)
		}
		if err := storage.AtomicWriteFile(clean, b); err != nil {
			return DefaultState(), fmt.Errorf("rewrite default queue state: %w", err)
		}
		return st, nil
	}

	// Нормализуем инварианты и при необходимости лечим файл.
	fixed := false
	if st.NextID <= 0 {
		st.NextID = 1
		fixed = true
	}
	if st.Regular == nil {
		st.Regular = make([]Job, 0)
		fixed = true
	}
	if st.Urgent == nil {
		st.Urgent = make([]Job, 0)
		fixed = true
	}
	if fixed {
		b, errJSON := json.MarshalIndent(st, "", "  ")
		if errJSON != nil {
			return st, fmt.Errorf("encode fixed queue state: %w", errJSON)
		}
		if err := storage.AtomicWriteFile(clean, b); err != nil {
			return st, fmt.Errorf("persist fixed queue state: %w", err)
		}
	}
	return st, nil
}

// Start запускает фоновую горутину persist-воркера. Повторные вызовы безопасно игнорируются.
func (s *QueueStore) Start() {
	s.startOnce.Do(func() {
		s.wg.Go(func() {
			s.loop()
		})
	})
}

// Stop корректно завершает фоновую запись и дожидается её окончания.
// Все отложенные записи будут выполнены. Возвращает первую ошибку записи, если была.
func (s *QueueStore) Stop() error {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
	return s.finalError()
}

// Load читает текущий снимок состояния из файла, при необходимости лечит его через ensureStateFile().
func (s *QueueStore) Load() (State, error) {
	return ensureStateFile(s.path)
}

// SchedulePersist ставит новое состояние в очередь на запись. Срезы клонируются,
// а буфер updates хранит только один актуальный снапшот: устаревшие заменяются.
func (s *QueueStore) SchedulePersist(state State) {
	clone := state.Clone()
	for {
		select {
		case <-s.stopCh:
			return
		// Случай успешной отправки: сохраняем актуальное состояние в буфер.
		case s.updates <- clone:
			return
		default:
			select {
			case <-s.stopCh:
				return
			case <-s.updates:
			default:
			}
		}
	}
}

// loop — главный цикл: накапливает pending, перезапускает таймер дебаунса,
// пишет снапшот по таймеру, по Flush или на остановке. Все записи идут через writeState().
func (s *QueueStore) loop() {
	var pending *State

	timer := time.NewTimer(s.debounce)
	timer.Stop()

	defer logger.Debug("QueueStore: loop exited")

	for {
		select {
		case state := <-s.updates:
			// state уже пришёл как Clone() из SchedulePersist
			pending = &state
			stopAndDrainTimer(timer)
			timer.Reset(s.debounce)

		case <-timer.C:
			s.consumePending(&pending)

		case <-s.stopCh:
			stopAndDrainTimer(timer)
			s.consumePending(&pending)
			return
		}
	}
}

// stopAndDrainTimer останавливает таймер и осушает его канал, если нужно.
func stopAndDrainTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

// consumePending записывает pending состояние, если оно есть, и обнуляет его.
func (s *QueueStore) consumePending(pending **State) {
	if *pending != nil {
		if err := s.writeState(**pending); err != nil {
			s.setFinalErr(err)
		}
		*pending = nil
	}
}

// FailedStore — отдельный журнал окончательно провалившихся заданий.
// Запись производится атомарно; доступ защищён mutex.
type FailedStore struct {
	path string
	mu   sync.Mutex
}

// NewFailedStore создаёт файл, если его нет (инициализирует пустым JSON-массивом "[]").
func NewFailedStore(path string) (*FailedStore, error) {
	clean := filepath.Clean(path)
	if _, err := os.Stat(clean); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if errFile := storage.AtomicWriteFile(clean, []byte("[]")); errFile != nil {
				return nil, fmt.Errorf("init failed store file: %w", errFile)
			}
			logger.Debugf("FailedStore: created file %s", clean)
		} else {
			return nil, fmt.Errorf("stat failed store: %w", err)
		}
	}
	return &FailedStore{path: clean}, nil
}

// Load возвращает все записи журнала. Пустой файл или отсутствие файла трактуются как пустой список.
func (s *FailedStore) Load() ([]FailedRecord, error) {
	bytes, errRead := os.ReadFile(s.path)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read failed store: %w", errRead)
	}
	if len(bytes) == 0 {
		return nil, nil
	}
	var records []FailedRecord
	if err := json.Unmarshal(bytes, &records); err != nil {
		return nil, fmt.Errorf("decode failed store: %w", err)
	}
	return records, nil
}

// Append добавляет записи в журнал. Делает Clone() для надёжности и атомарно записывает весь массив.
func (s *FailedStore) Append(records ...FailedRecord) error {
	if len(records) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, errLoad := s.Load()
	if errLoad != nil {
		return errLoad
	}
	for _, record := range records {
		existing = append(existing, record.Clone())
	}

	data, errJSON := json.MarshalIndent(existing, "", "  ")
	if errJSON != nil {
		return fmt.Errorf("encode failed store: %w", errJSON)
	}
	if err := storage.AtomicWriteFile(s.path, data); err != nil {
		logger.Errorf("FailedStore: write error: %v", err)
		return err
	}
	logger.Debugf("FailedStore: appended %d record(s)", len(records))
	return nil
}

// writeState кодирует state в JSON и атомарно записывает на диск. Логирует размеры очередей.
func (s *QueueStore) writeState(state State) error {
	data, errJSON := json.MarshalIndent(state, "", "  ")
	if errJSON != nil {
		logger.Errorf("QueueStore: marshal error: %v", errJSON)
		return fmt.Errorf("encode queue state: %w", errJSON)
	}
	if err := storage.AtomicWriteFile(s.path, data); err != nil {
		logger.Errorf("QueueStore: write error: %v", err)
		return err
	}
	logger.Debugf("QueueStore: state persisted (%d regular, %d urgent)", len(state.Regular), len(state.Urgent))
	return nil
}

func (s *QueueStore) setFinalErr(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	if s.finalErr == nil {
		s.finalErr = err
	}
	s.errMu.Unlock()
}

// finalError возвращает сохранённую первую ошибку записи. Потокобезопасно.
func (s *QueueStore) finalError() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.finalErr
}
