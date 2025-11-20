package web

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// AuthManager управляет токенами и сессиями веб-интерфейса
type AuthManager struct {
	mu         sync.RWMutex
	token      string              // текущий активный токен
	sessions   map[string]*Session // sessionID -> Session
	sessionTTL time.Duration       // время жизни сессии
}

// Session представляет активную сессию пользователя
type Session struct {
	ID        string
	CreatedAt time.Time
	LastSeen  time.Time
}

// NewAuthManager создает новый менеджер аутентификации
func NewAuthManager(sessionTTL time.Duration) *AuthManager {
	return &AuthManager{
		sessions:   make(map[string]*Session),
		sessionTTL: sessionTTL,
	}
}

// GenerateToken генерирует новый токен и удаляет все старые сессии
func (am *AuthManager) GenerateToken() string {
	am.mu.Lock()
	defer am.mu.Unlock()

	// Генерируем новый токен
	am.token = uuid.New().String()

	// Удаляем все старые сессии
	am.sessions = make(map[string]*Session)

	return am.token
}

// ValidateToken проверяет токен и создает новую сессию
func (am *AuthManager) ValidateToken(token string) (string, bool) {
	am.mu.Lock()
	defer am.mu.Unlock()

	if token == "" || am.token == "" || token != am.token {
		return "", false
	}

	// Создаем новую сессию
	sessionID := uuid.New().String()
	now := time.Now()
	am.sessions[sessionID] = &Session{
		ID:        sessionID,
		CreatedAt: now,
		LastSeen:  now,
	}

	return sessionID, true
}

// ValidateSession проверяет сессию и обновляет LastSeen
func (am *AuthManager) ValidateSession(sessionID string) bool {
	am.mu.Lock()
	defer am.mu.Unlock()

	session, exists := am.sessions[sessionID]
	if !exists {
		return false
	}

	// Проверяем, не истекла ли сессия
	if time.Since(session.LastSeen) > am.sessionTTL {
		delete(am.sessions, sessionID)
		return false
	}

	// Обновляем LastSeen
	session.LastSeen = time.Now()
	return true
}

// InvalidateSession удаляет сессию
func (am *AuthManager) InvalidateSession(sessionID string) {
	am.mu.Lock()
	defer am.mu.Unlock()
	delete(am.sessions, sessionID)
}

// CleanExpiredSessions удаляет истекшие сессии
func (am *AuthManager) CleanExpiredSessions() {
	am.mu.Lock()
	defer am.mu.Unlock()

	now := time.Now()
	for id, session := range am.sessions {
		if now.Sub(session.LastSeen) > am.sessionTTL {
			delete(am.sessions, id)
		}
	}
}

// GetCurrentToken возвращает текущий активный токен (для отладки)
func (am *AuthManager) GetCurrentToken() string {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return am.token
}

// DeleteCurrentToken удаляет текущий активный токен
func (am *AuthManager) DeleteCurrentToken() {
	am.mu.RLock()
	defer am.mu.RUnlock()
	am.token = ""
}
