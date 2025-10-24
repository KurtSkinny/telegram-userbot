// Package lifecycle — менеджер управляемых подсистем приложения.
// Поддерживает иерархию контекстов, явные зависимости между узлами и гарантирует
// предсказуемый порядок запуска/остановки. Менеджер упрощает построение «дерева»
// сервисов, где каждая ветка наследует отмену контекста и корректно гасится при Shutdown.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"telegram-userbot/internal/infra/logger"
)

// StartFunc запускает узел и может вернуть контекст, который станет родительским
// для дочерних узлов. Если возвращён nil, менеджер использует свой дочерний
// контекст. Ошибка приводит к пометке узла как failed и прерыванию его старта.
type StartFunc func(ctx context.Context) (context.Context, error)

// StopFunc останавливает узел. На момент вызова контекст узла уже отменён,
// поэтому реализация должна корректно завершить фоновые задачи и освободить ресурсы.
type StopFunc func(ctx context.Context) error

// nodeStatus описывает текущее состояние узла в жизненном цикле менеджера.
type nodeStatus int

const (
	statusRegistered nodeStatus = iota // зарегистрирован, ещё не запускался
	statusStarting                     // идёт запуск или ожидание зависимостей
	statusRunning                      // успешно запущен, контекст активен
	statusStopping                     // получена команда на остановку, контекст отменён
	statusStopped                      // корректно остановлен
	statusFailed                       // ошибка при запуске/остановке
)

const rootName = "root"

type node struct {
	name   string
	parent string
	deps   []string

	start StartFunc
	stop  StopFunc

	ctx    context.Context
	cancel context.CancelFunc
	status nodeStatus
	err    error
}

// Manager управляет жизненным циклом набора узлов и гарантирует корректный порядок
// запуска/остановки с учётом зависимостей и иерархии контекстов. Потокобезопасен.
type Manager struct {
	mu         sync.Mutex       // защищает доступ к nodes/startOrder
	nodes      map[string]*node // все зарегистрированные узлы, включая root
	startOrder []string         // фактический порядок запуска, нужен для обратной остановки
}

// Logger — минимальный интерфейс логирования, используемый менеджером.
type Logger interface {
	Debugf(format string, args ...interface{})
	Infof(format string, args ...interface{})
	Warnf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
}

// New создаёт менеджер с корневым узлом root, уже находящимся в состоянии Running.
// Если rootCtx=nil, используется context.Background(). Root выступает невидимым
// родителем для остальных узлов и задаёт их жизненный цикл.
func New(rootCtx context.Context) *Manager {
	if rootCtx == nil {
		rootCtx = context.Background()
	}

	rootNode := &node{
		name:   rootName,
		parent: "",
		deps:   nil,
		ctx:    rootCtx,
		cancel: nil,
		status: statusRunning,
	}

	return &Manager{
		nodes: map[string]*node{
			rootName: rootNode,
		},
	}
}

// Register добавляет новый узел name. Если parent пуст, используется root.
// deps — дополнительные зависимости, которые должны быть запущены ДО текущего узла.
// Проверки: уникальность имени, наличие родителя, удаление дубликатов/parent из deps,
// запрет зависимости от самого себя. Узел регистрируется в состоянии Registered.
func (m *Manager) Register(name string, parent string, deps []string, start StartFunc, stop StopFunc) error {
	if name == "" || name == rootName {
		return fmt.Errorf("lifecycle: invalid node name %q", name)
	}
	if parent == "" {
		// По умолчанию привязываем узел к корню дерева контекстов.
		parent = rootName
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.nodes[name]; exists {
		return fmt.Errorf("lifecycle: node %q already registered", name)
	}
	if _, parentExists := m.nodes[parent]; !parentExists {
		return fmt.Errorf("lifecycle: parent %q not found for node %q", parent, name)
	}

	// Удаляем дубликаты и не позволяем зависеть от родителя (он и так выше по иерархии).
	uniqueDeps := slices.Compact(slices.Clone(deps))
	uniqueDeps = slices.DeleteFunc(uniqueDeps, func(d string) bool { return d == parent })
	if slices.Contains(uniqueDeps, name) {
		return fmt.Errorf("lifecycle: node %q cannot depend on itself", name)
	}

	m.nodes[name] = &node{
		name:   name,
		parent: parent,
		deps:   uniqueDeps,
		start:  start,
		stop:   stop,
		status: statusRegistered,
	}
	return nil
}

// StartAll запускает все зарегистрированные узлы (кроме root) с учётом зависимостей.
// Порядок запуска детерминирован: имена сортируются по алфавиту, но фактический
// порядок фиксируется в startOrder после учёта рекурсивного старта родителей/зависимостей.
// Возвращает объединённую ошибку, если какие-то узлы не стартовали.
func (m *Manager) StartAll() error {
	m.mu.Lock()
	names := make([]string, 0, len(m.nodes))
	for name := range m.nodes {
		if name != rootName {
			names = append(names, name)
		}
	}
	m.mu.Unlock()

	// Делаем предсказуемый проход по именам, чтобы логи были стабильны.
	slices.Sort(names)

	var errs error
	for _, name := range names {
		if err := m.startNode(name); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	// Запоминаем и логируем итоговый порядок — он нужен для корректного Shutdown.
	logger.Debugf("lifecycle start order: %v", m.startOrder)
	return errs
}

// startNode рекурсивно запускает узел: гарантирует запуск родителя и всех deps,
// создаёт дочерний контекст и, при необходимости, «мостит» его с контекстом,
// возвращённым StartFunc. Защищается от циклов: повторный вход в Starting трактуется
// как цикл зависимостей.
func (m *Manager) startNode(name string) error {
	m.mu.Lock()
	n, exists := m.nodes[name]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("lifecycle: node %q not registered", name)
	}

	// Мини‑машина состояний узла. Повторный вход в Starting означает цикл.
	switch n.status { //nolint:exhaustive // // статус-машина полная, но не все состояния ветвятся сейчас
	case statusRunning:
		m.mu.Unlock()
		return nil
	case statusStarting:
		m.mu.Unlock()
		return fmt.Errorf("lifecycle: detected cycle while starting %q", name)
	}
	n.status = statusStarting
	m.mu.Unlock()

	logger.Debugf("starting node %s", name)

	if n.parent != "" {
		if err := m.startNode(n.parent); err != nil {
			m.setNodeFailed(name, err)
			logger.Errorf("failed to start node %s: %v", name, err)
			return err
		}
	}
	// Гарантируем, что все зависимости подняты до текущего узла.
	for _, dep := range n.deps {
		if err := m.startNode(dep); err != nil {
			m.setNodeFailed(name, err)
			logger.Errorf("failed to start node %s: %v", name, err)
			return err
		}
	}

	parentCtx, err := m.nodeContext(n.parent)
	if err != nil {
		m.setNodeFailed(name, err)
		return err
	}

	// Наследуем отмену родителя и даём узлу собственный cancel.
	childCtx, cancel := context.WithCancel(parentCtx)
	finalCtx := childCtx

	if n.start != nil {
		if startedCtx, errStart := n.start(childCtx); errStart != nil {
			cancel()
			m.setNodeFailed(name, errStart)
			return errStart
		} else if startedCtx != nil && startedCtx != childCtx {
			// Узел вернул производный контекст. Привязываем его отмену к отмене childCtx,
			// чтобы Shutdown корректно гасил поддерево даже при обёртках.
			// Узел вернул «внешний» контекст. Делаем прокладку, чтобы наш cancel гарантированно его гасил.
			bridged, bridgedCancel := context.WithCancel(startedCtx)
			// Если сначала отменится наш дочерний контекст — погасим и обёрнутый.
			stopAfter := context.AfterFunc(childCtx, bridgedCancel)

			// Переопределяем cancel так, чтобы закрыть обе ветки и остановить отложенную функцию.
			oldCancel := cancel
			cancel = func() {
				oldCancel()
				// останавливаем отложенную функцию, если ещё не сработала
				stopAfter()
				// гарантируем отмену обёрнутого контекста
				bridgedCancel()
			}
			finalCtx = bridged
		}
	}

	m.mu.Lock()
	n.ctx = finalCtx
	n.cancel = cancel
	n.status = statusRunning
	n.err = nil
	// Фиксируем порядок запуска, исключая дубликаты (узел мог быть поднят как зависимость).
	if !slices.Contains(m.startOrder, name) {
		m.startOrder = append(m.startOrder, name)
	}
	m.mu.Unlock()

	logger.Debugf("node %s is running", name)

	return nil
}

// nodeContext возвращает контекст узла либо ошибку, если узел не найден
// или ещё не получил контекст (не стартовал).
func (m *Manager) nodeContext(name string) (context.Context, error) {
	if name == "" {
		name = rootName
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	n, ok := m.nodes[name]
	if !ok {
		return nil, fmt.Errorf("node %q not registered", name)
	}
	if n.ctx == nil {
		return nil, fmt.Errorf("node %q has no context", name)
	}
	return n.ctx, nil
}

// Shutdown останавливает все запущенные узлы в порядке, обратном фактическому старту.
// Это гарантирует, что дочерние узлы гаснут раньше родителей. Возвращает объединённую
// ошибку, если какие‑то stop‑хуки отработали с ошибкой.
func (m *Manager) Shutdown() error {
	m.mu.Lock()
	order := append([]string(nil), m.startOrder...)
	m.mu.Unlock()
	logger.Debugf("shutdown order: %v", order)

	var errs error
	for i := len(order) - 1; i >= 0; i-- {
		name := order[i]
		if err := m.stopNode(name); err != nil {
			errs = errors.Join(errs, err)
		}
		logger.Debugf("node %s stop processed", name)
	}
	return errs
}

// stopNode останавливает узел в состоянии Running: отменяет контекст, вызывает StopFunc
// и переводит состояние в Stopped/Failed в зависимости от результата.
func (m *Manager) stopNode(name string) error {
	m.mu.Lock()
	n, exists := m.nodes[name]
	if !exists || n.status != statusRunning {
		m.mu.Unlock()
		return nil
	}
	n.status = statusStopping
	cancel := n.cancel
	stopFn := n.stop
	nodeCtx := n.ctx
	m.mu.Unlock()

	logger.Debugf("stopping node %s", name)

	// Сначала отменяем контекст — корректный сигнал для фоновых горутин узла.
	if cancel != nil {
		cancel()
	}

	var err error
	if stopFn != nil {
		err = stopFn(nodeCtx)
	}

	m.mu.Lock()
	if err != nil {
		n.status = statusFailed
		n.err = err
	} else {
		n.status = statusStopped
		n.err = nil
	}
	m.mu.Unlock()

	if err != nil {
		logger.Errorf("node %s stopped with error: %v", name, err)
	} else {
		logger.Debugf("node %s stopped", name)
	}
	return err
}

// setNodeFailed помечает узел как Failed и сохраняет ошибку под мьютексом.
func (m *Manager) setNodeFailed(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if n, ok := m.nodes[name]; ok {
		n.status = statusFailed
		n.err = err
	}
}
