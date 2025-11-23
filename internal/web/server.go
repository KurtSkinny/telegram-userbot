package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"telegram-userbot/internal/commands"
	"telegram-userbot/internal/config"
	"telegram-userbot/internal/logger"

	"go.uber.org/zap"
)

// Server представляет веб-сервер
type Server struct {
	srv          *http.Server
	auth         *AuthManager
	executor     commands.Executor
	tmpl         *template.Template
	logsTemplate *template.Template
	ctx          context.Context
	cancel       context.CancelFunc
}

const (
	readTimeout  = 15 * time.Second
	writeTimeout = 15 * time.Second
	idleTimeout  = 60 * time.Second

	cleanExpiredSessionsInterval = 3 * time.Minute
)

// NewServer создает новый веб-сервер
func NewServer(executor commands.Executor) *Server {
	// Создаем auth manager с TTL 1 час
	auth := NewAuthManager(time.Hour)

	s := &Server{
		auth:     auth,
		executor: executor,
	}

	// Загружаем шаблоны
	s.loadTemplates()

	// Настраиваем роутинг
	mux := http.NewServeMux()

	// Публичные эндпоинты (без авторизации)
	mux.HandleFunc("/health", s.handleHealth)

	// Защищенные эндпоинты (требуют авторизации)
	protected := http.NewServeMux()
	protected.HandleFunc("/", s.handleDashboard)
	protected.HandleFunc("/logs", s.handleLogs)
	protected.HandleFunc("/filters", s.handleFilters)
	protected.HandleFunc("/recipients", s.handleRecipients)

	// API эндпоинты для HTMX
	protected.HandleFunc("/api/status", s.handleAPIStatus)
	protected.HandleFunc("/api/list", s.handleAPIList)
	protected.HandleFunc("/api/flush", s.handleAPIFlush)
	protected.HandleFunc("/api/refresh", s.handleAPIRefresh)
	protected.HandleFunc("/api/reload", s.handleAPIReload)
	protected.HandleFunc("/api/test", s.handleAPITest)
	protected.HandleFunc("/api/logs", s.handleAPILogs)
	protected.HandleFunc("/api/whoami", s.handleAPIWhoami)
	protected.HandleFunc("/api/version", s.handleAPIVersion)

	// Применяем middleware
	mux.Handle("/", s.authMiddleware(protected))

	// HTTP сервер
	addr := config.Env().WebServerAddress
	s.srv = &http.Server{
		Addr:         addr,
		Handler:      loggingMiddleware(mux),
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
	}

	return s
}

// Start запускает веб-сервер
func (s *Server) Start() error {
	logger.Info("Starting web server", zap.String("address", s.srv.Addr))

	// Запускаем фоновую очистку истекших сессий
	s.ctx, s.cancel = context.WithCancel(context.Background())
	go s.cleanupLoop(s.ctx)

	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("web server error: %w", err)
	}
	return nil
}

// Shutdown корректно останавливает веб-сервер
func (s *Server) Shutdown(ctx context.Context) error {
	logger.Info("Shutting down web server...")
	if s.cancel != nil {
		s.cancel()
	}
	return s.srv.Shutdown(ctx)
}

// cleanupLoop периодически очищает истекшие сессии
func (s *Server) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(cleanExpiredSessionsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.auth.CleanExpiredSessions()
		}
	}
}

// GetAuthToken возвращает текущий токен авторизации
func (s *Server) GetAuthToken() string {
	return s.auth.GetCurrentToken()
}

// GenerateAuthToken генерирует новый токен авторизации
func (s *Server) GenerateAuthToken() string {
	token := s.auth.GenerateToken()
	logger.Info("Generated new auth token for web interface")
	return token
}

// handleHealth проверка здоровья сервера
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	writeResponse(w, []byte("OK"))
}

// loadTemplates загружает HTML шаблоны
func (s *Server) loadTemplates() {
	// Основные шаблоны страниц
	s.tmpl = template.Must(template.New("").Parse(layoutTemplate))
	template.Must(s.tmpl.Parse(dashboardTemplate))
	template.Must(s.tmpl.Parse(logsTemplate))
	template.Must(s.tmpl.Parse(filtersTemplate))
	template.Must(s.tmpl.Parse(recipientsTemplate))

	// Шаблоны для логов с helper функциями
	s.logsTemplate = template.Must(template.New("").Funcs(templateFuncs).Parse(logsEntryTemplate))
	template.Must(s.logsTemplate.Parse(logsPaginationTemplate))
	template.Must(s.logsTemplate.Parse(logsContainerTemplate))
}
