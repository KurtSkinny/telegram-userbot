package web

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"telegram-userbot/internal/infra/logger"
)

// PageData - данные для рендеринга страницы
type PageData struct {
	Title string
	Page  string
	Data  any
}

const (
	shortTimeOut  = 5 * time.Second
	mediumTimeOut = 30 * time.Second
	longTimeOut   = 120 * time.Second
)

// handleDashboard отображает главную страницу
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data := PageData{
		Title: "Dashboard",
		Page:  "dashboard",
	}

	if err := s.tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		logger.Errorf("Error rendering dashboard: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// handleLogs отображает страницу логов
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	data := PageData{
		Title: "Logs",
		Page:  "logs",
	}

	if err := s.tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		logger.Errorf("Error rendering logs: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// handleFilters отображает страницу управления фильтрами (заглушка)
func (s *Server) handleFilters(w http.ResponseWriter, r *http.Request) {
	data := PageData{
		Title: "Filters",
		Page:  "filters",
	}

	if err := s.tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		logger.Errorf("Error rendering filters: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// handleRecipients отображает страницу управления получателями (заглушка)
func (s *Server) handleRecipients(w http.ResponseWriter, r *http.Request) {
	data := PageData{
		Title: "Recipients",
		Page:  "recipients",
	}

	if err := s.tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		logger.Errorf("Error rendering recipients: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// API Handlers

// handleAPIStatus возвращает статус очереди
func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), shortTimeOut)
	defer cancel()

	result, eErr := s.executor.Status(ctx)
	if eErr != nil {
		logger.Errorf("Status command failed: %v", eErr)
		writeResponse(w, []byte(fmt.Sprintf(`<p class="text-red-600">Error: %v</p>`, eErr)))
		return
	}

	html := fmt.Sprintf(`
		<div class="space-y-2">
			<p class="text-sm"><span class="font-semibold">Urgent Queue:</span> %d</p>
			<p class="text-sm"><span class="font-semibold">Regular Queue:</span> %d</p>
			<p class="text-sm"><span class="font-semibold">Last Drain:</span> %s</p>
			<p class="text-sm"><span class="font-semibold">Last Persist:</span> %s</p>
			<p class="text-sm"><span class="font-semibold">Next Schedule:</span> %s</p>
		</div>
	`,
		result.UrgentQueueSize,
		result.RegularQueueSize,
		formatTime(result.LastRegularDrainAt, result.Location),
		formatTime(result.LastFlushAt, result.Location),
		formatTime(result.NextScheduleAt, result.Location),
	)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	writeResponse(w, []byte(html))
}

// handleAPIList возвращает список диалогов
func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), shortTimeOut)
	defer cancel()

	result, eErr := s.executor.List(ctx)
	if eErr != nil {
		logger.Errorf("List command failed: %v", eErr)
		writeResponse(w, []byte(fmt.Sprintf(`<p class="text-red-600">Error: %v</p>`, eErr)))
		return
	}

	if len(result.Dialogs) == 0 {
		writeResponse(w, []byte(`<p class="text-gray-500">No dialogs cached yet.</p>`))
		return
	}

	html := `<div class="space-y-1 text-sm">`
	for _, d := range result.Dialogs {
		// if i >= 10 { // Показываем только первые 10
		// 	html += fmt.Sprintf(`<p class="text-gray-500 text-xs mt-2">... and %d more dialogs</p>`,
		// 		len(result.Dialogs)-10)
		// 	break
		// }

		icon := getDialogIcon(d.Kind)
		title := d.Title
		if title == "" {
			title = fmt.Sprintf("ID: %d", d.ID)
		}

		// Формат: Icon Title (@username) id: ID
		username := d.Username
		if username == "" || username == "-" {
			username = ""
		} else {
			username = fmt.Sprintf(" (@%s)", username)
		}

		html += fmt.Sprintf(
			`<p class="truncate">%s <span class="font-medium">%s</span>
			<span class="text-gray-500 text-xs">%s id: %d</span></p>`,
			icon, title, username, d.ID)
	}
	html += fmt.Sprintf(`<p class="text-xs text-gray-500 mt-2">Total: %d dialogs</p></div>`, len(result.Dialogs))
	// html += `</div>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	writeResponse(w, []byte(html))
}

// handleAPIFlush выполняет flush очереди
func (s *Server) handleAPIFlush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), shortTimeOut)
	defer cancel()

	err := s.executor.Flush(ctx)
	if err != nil {
		logger.Errorf("Flush command failed: %v", err)
		writeResponse(w, []byte(fmt.Sprintf(`<p class="text-red-600">Error: %v</p>`, err)))
		return
	}

	// Возвращаем обновленный статус
	s.handleAPIStatus(w, r)
}

// handleAPIRefresh обновляет диалоги
func (s *Server) handleAPIRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), mediumTimeOut)
	defer cancel()

	err := s.executor.RefreshDialogs(ctx)
	if err != nil {
		logger.Errorf("RefreshDialogs command failed: %v", err)
		writeResponse(w, []byte(fmt.Sprintf(`<p class="text-red-600">Error: %v</p>`, err)))
		return
	}

	// Возвращаем обновленный список
	s.handleAPIList(w, r)
}

// handleAPIReload перезагружает фильтры
func (s *Server) handleAPIReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), shortTimeOut)
	defer cancel()

	eErr := s.executor.ReloadFilters(ctx)
	if eErr != nil {
		logger.Errorf("ReloadFilters command failed: %v", eErr)
		writeResponse(w, []byte(fmt.Sprintf(`<p class="text-red-600">Error: %v</p>`, eErr)))
		return
	}

	html := `<div class="space-y-2">
		<p class="text-green-600 font-semibold">✓ Filters reloaded successfully</p>
		<p class="text-sm text-gray-500">Configure filters in <code class="bg-gray-100 px-1">assets/filters.json</code></p>
	</div>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	writeResponse(w, []byte(html))
}

// handleAPITest выполняет тест соединения
func (s *Server) handleAPITest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), longTimeOut)
	defer cancel()

	result, eErr := s.executor.Test(ctx)
	if eErr != nil {
		logger.Errorf("Test command failed: %v", eErr)
		writeResponse(w, []byte(fmt.Sprintf(`<p class="text-red-600">Error: %v</p>`, eErr)))
		return
	}

	var html string
	if result.Success {
		html = fmt.Sprintf(`<div class="space-y-2 mt-4">
			<p class="text-green-600 font-semibold">✓ Test message sent successfully</p>
			<p class="text-sm text-gray-600">%s</p>
			<p class="text-xs text-gray-500">Sent at: %s</p>
		</div>`, result.Message, result.SentAt.Format(time.RFC3339))
	} else {
		html = `<p class="text-red-600">✗ Test failed</p>`
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	writeResponse(w, []byte(html))
}

// handleAPIWhoami возвращает информацию о текущем аккаунте
func (s *Server) handleAPIWhoami(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), shortTimeOut)
	defer cancel()

	result, eErr := s.executor.Whoami(ctx)
	if eErr != nil {
		logger.Errorf("Whoami command failed: %v", eErr)
		writeResponse(w, []byte(fmt.Sprintf(`<p class="text-red-600">Error: %v</p>`, eErr)))
		return
	}

	var html string
	html += fmt.Sprintf(`<p class="text-sm"><span class="font-semibold">Name:</span> %s</p>`, result.FullName)
	if result.Username != "" {
		html += fmt.Sprintf(`<p class="text-sm"><span class="font-semibold">Username:</span> @%s</p>`, result.Username)
	}
	html += fmt.Sprintf(`<p class="text-sm"><span class="font-semibold">ID:</span> %d</p>`, result.ID)

	html = `<div class="space-y-2">` + html + `</div>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	writeResponse(w, []byte(html))
}

// handleAPIVersion возвращает версию приложения
func (s *Server) handleAPIVersion(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), shortTimeOut)
	defer cancel()

	result, verErr := s.executor.Version(ctx)
	if verErr != nil {
		logger.Errorf("Version command failed: %v", verErr)
		writeResponse(w, []byte(fmt.Sprintf(`<p class="text-red-600">Error: %v</p>`, verErr)))
		return
	}

	html := fmt.Sprintf(`<p class="text-sm"><span class="font-semibold">Version:</span> %s v%s</p>`,
		result.Name, result.Version)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	writeResponse(w, []byte(html))
}

// Вспомогательные функции

func formatTime(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "<never>"
	}
	return t.In(loc).Format("2006-01-02 15:04:05")
}

func getDialogIcon(kind string) string {
	switch kind {
	case "user":
		return "👤"
	case "chat":
		return "👥"
	case "channel":
		return "📢"
	case "folder":
		return "📁"
	default:
		return "❓"
	}
}
