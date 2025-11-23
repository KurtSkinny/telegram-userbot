package web

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"telegram-userbot/internal/config"
	"telegram-userbot/internal/logger"
	"telegram-userbot/internal/timeutil"
)

// LogEntry представляет одну запись лога
type LogEntry struct {
	Timestamp string
	Level     string
	Caller    string
	Message   string
}

const (
	logsPageSize       = 1000
	paginationMaxPages = 100
)

// handleAPILogs возвращает логи с пагинацией
func (s *Server) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	// Парсим номер страницы
	page := parsePage(r)

	// Читаем логи
	logs, totalPages, readErr := s.readLogs(page, logsPageSize)
	if readErr != nil {
		logger.Errorf("Failed to read logs: %v", readErr)
		writeResponse(w, []byte(fmt.Sprintf(`<p class="text-red-600">Error reading logs: %v</p>`, readErr)))
		return
	}

	if len(logs) == 0 {
		writeResponse(w, []byte(`<p class="text-gray-500">No logs available</p>`))
		return
	}

	// Подготавливаем данные для шаблона
	data := LogsPageData{
		Entries:    make([]LogEntryWithClass, len(logs)),
		Pagination: buildPagination(page, totalPages),
	}

	// Добавляем CSS классы к записям
	for i, entry := range logs {
		data.Entries[i] = LogEntryWithClass{
			LogEntry:   entry,
			LevelClass: getLevelClass(entry.Level),
		}
	}

	// Рендерим шаблон
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.logsTemplate.ExecuteTemplate(w, "logs-container", data); err != nil {
		logger.Errorf("Failed to render logs template: %v", err)
		writeResponse(w, []byte(fmt.Sprintf(`<p class="text-red-600">Template error: %v</p>`, err)))
	}
}

// parsePage извлекает номер страницы из запроса
func parsePage(r *http.Request) int {
	pageStr := r.URL.Query().Get("page")
	if pageStr == "" {
		return 1
	}

	page, err := strconv.Atoi(pageStr)
	if err != nil || page < 1 {
		return 1
	}

	if page > paginationMaxPages {
		return paginationMaxPages
	}

	return page
}

// readLogs читает логи из файла с пагинацией (100 МБ макс)
func (s *Server) readLogs(page, pageSize int) ([]LogEntry, int, error) {
	logFile := config.Env().LogFile
	if logFile == "" {
		return nil, 0, errors.New("log file not configured")
	}

	file, openErr := os.Open(logFile)
	if openErr != nil {
		return nil, 0, fmt.Errorf("failed to open log file: %w", openErr)
	}
	defer file.Close()

	stat, statErr := file.Stat()
	if statErr != nil {
		return nil, 0, fmt.Errorf("failed to stat log file: %w", statErr)
	}

	const maxLogFileSize = 100 * 1024 * 1024 // 100MB
	if stat.Size() > maxLogFileSize {
		return nil, 0, fmt.Errorf("log file too large: %d bytes (max %d), consider log rotation",
			stat.Size(), maxLogFileSize)
	}

	// Читаем все строки для подсчета общего количества
	var allLines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return nil, 0, fmt.Errorf("failed to read log file: %w", err)
	}

	// Переворачиваем, чтобы новые были сверху
	for i, j := 0, len(allLines)-1; i < j; i, j = i+1, j-1 {
		allLines[i], allLines[j] = allLines[j], allLines[i]
	}

	totalLines := len(allLines)
	totalPages := (totalLines + pageSize - 1) / pageSize

	// Вычисляем диапазон для текущей страницы
	start := (page - 1) * pageSize
	end := min(start+pageSize, totalLines)

	if start >= totalLines {
		return []LogEntry{}, totalPages, nil
	}

	// Парсим строки для текущей страницы
	var entries []LogEntry
	for i := start; i < end; i++ {
		entry, err := parseLogLine(allLines[i])
		if err != nil {
			// Если не удалось распарсить, показываем как есть
			entries = append(entries, LogEntry{
				Timestamp: "",
				Level:     "UNKNOWN",
				Caller:    "",
				Message:   allLines[i],
			})
			continue
		}
		entries = append(entries, entry)
	}

	return entries, totalPages, nil
}

// parseLogLine парсит строку NDJSON лога
func parseLogLine(line string) (LogEntry, error) {
	var raw struct {
		Level  string `json:"level"`
		Time   string `json:"time"` // Zap использует "time"
		TS     string `json:"ts"`   // Альтернативное поле
		Caller string `json:"caller"`
		Msg    string `json:"msg"`
	}

	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return LogEntry{}, err
	}

	// Парсим timestamp (приоритет у "time")
	ts := ""
	timeStr := raw.Time
	if timeStr == "" {
		timeStr = raw.TS
	}

	if timeStr != "" {
		ts = timeutil.NormalizeLogTimestamp(timeStr, time.Local)
	}

	return LogEntry{
		Timestamp: ts,
		Level:     normalizeLevel(raw.Level),
		Caller:    raw.Caller,
		Message:   raw.Msg,
	}, nil
}

// normalizeLevel приводит уровень к верхнему регистру
func normalizeLevel(level string) string {
	switch level {
	case "debug":
		return "DEBUG"
	case "info":
		return "INFO"
	case "warn", "warning":
		return "WARN"
	case "error":
		return "ERROR"
	default:
		return level
	}
}

// getLevelClass возвращает CSS класс для уровня лога
func getLevelClass(level string) string {
	switch level {
	case "ERROR":
		return "bg-red-50 text-red-800 border-l-4 border-red-500"
	case "WARN":
		return "bg-yellow-50 text-yellow-800 border-l-4 border-yellow-500"
	case "INFO":
		return "bg-blue-50 text-blue-800 border-l-4 border-blue-500"
	case "DEBUG":
		return "bg-gray-50 text-gray-600 border-l-4 border-gray-400"
	default:
		return "bg-gray-50 text-gray-800"
	}
}
