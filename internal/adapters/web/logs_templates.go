package web

import "html/template"

// LogsPageData - данные для рендеринга страницы логов
type LogsPageData struct {
	Entries    []LogEntryWithClass
	Pagination PaginationData
}

// PaginationData - данные для пагинации
type PaginationData struct {
	CurrentPage int
	TotalPages  int
	ShowFirst   bool
	ShowPrev    bool
	ShowNext    bool
	ShowLast    bool
	Pages       []PageLink
}

// PageLink - ссылка на страницу
type PageLink struct {
	Number     int
	IsCurrent  bool
	IsEllipsis bool
}

// logsEntryTemplate - шаблон для одной записи лога
const logsEntryTemplate = `{{define "log-entry"}}
<div class="text-xs p-1 rounded {{.LevelClass}}">
	<span class="text-gray-400">[{{.Timestamp}}]</span>
	<span class="font-semibold">[{{.Level}}]</span>
	<span class="text-gray-500">[{{.Caller}}]</span>
	<span>{{.Message}}</span>
</div>
{{end}}`

// logsPaginationTemplate - шаблон для пагинации
const logsPaginationTemplate = `{{define "pagination"}}
{{if gt .TotalPages 1}}
<div class="mt-6 flex justify-center items-center space-x-2">
	{{if .ShowFirst}}
	<a href="#" hx-get="/api/logs?page=1" hx-target="#logs-container" hx-swap="innerHTML"
	   class="px-3 py-1 rounded bg-gray-200 hover:bg-gray-300">&lt;&lt;</a>
	{{end}}
	
	{{if .ShowPrev}}
	<a href="#" hx-get="/api/logs?page={{sub .CurrentPage 1}}" hx-target="#logs-container" hx-swap="innerHTML"
	   class="px-3 py-1 rounded bg-gray-200 hover:bg-gray-300">&lt;</a>
	{{end}}
	
	{{range .Pages}}
		{{if .IsEllipsis}}
			<span class="px-2">...</span>
		{{else if .IsCurrent}}
			<span class="px-3 py-1 rounded bg-blue-600 text-white">{{.Number}}</span>
		{{else}}
			<a href="#" hx-get="/api/logs?page={{.Number}}" hx-target="#logs-container" hx-swap="innerHTML"
			   class="px-3 py-1 rounded bg-gray-200 hover:bg-gray-300">{{.Number}}</a>
		{{end}}
	{{end}}
	
	{{if .ShowNext}}
	<a href="#" hx-get="/api/logs?page={{add .CurrentPage 1}}" hx-target="#logs-container" hx-swap="innerHTML"
	   class="px-3 py-1 rounded bg-gray-200 hover:bg-gray-300">&gt;</a>
	{{end}}
	
	{{if .ShowLast}}
	<a href="#" hx-get="/api/logs?page={{.TotalPages}}" hx-target="#logs-container" hx-swap="innerHTML"
	   class="px-3 py-1 rounded bg-gray-200 hover:bg-gray-300">&gt;&gt;</a>
	{{end}}
</div>
{{end}}
{{end}}`

// logsContainerTemplate - шаблон для контейнера логов
const logsContainerTemplate = `{{define "logs-container"}}
<div class="space-y-1">
{{range .Entries}}
	{{template "log-entry" .}}
{{end}}
</div>
{{template "pagination" .Pagination}}
{{end}}`

// buildPagination создает данные для пагинации
func buildPagination(currentPage, totalPages int) PaginationData {
	const window = 2 // количество страниц вокруг текущей

	pagination := PaginationData{
		CurrentPage: currentPage,
		TotalPages:  totalPages,
		ShowFirst:   currentPage > 1,
		ShowPrev:    currentPage > 1,
		ShowNext:    currentPage < totalPages,
		ShowLast:    currentPage < totalPages,
		Pages:       []PageLink{},
	}

	// Вычисляем диапазон отображаемых страниц
	start := max(1, currentPage-window)
	end := min(totalPages, currentPage+window)

	// Добавляем эллипсис в начале, если нужно
	if start > 1 {
		pagination.Pages = append(pagination.Pages, PageLink{IsEllipsis: true})
	}

	// Добавляем страницы
	for i := start; i <= end; i++ {
		pagination.Pages = append(pagination.Pages, PageLink{
			Number:    i,
			IsCurrent: i == currentPage,
		})
	}

	// Добавляем эллипсис в конце, если нужно
	if end < totalPages {
		pagination.Pages = append(pagination.Pages, PageLink{IsEllipsis: true})
	}

	return pagination
}

// LogEntryWithClass - запись лога с CSS классом
type LogEntryWithClass struct {
	LogEntry

	LevelClass string
}

// Helper functions для шаблонов
var templateFuncs = template.FuncMap{
	"sub": func(a, b int) int { return a - b },
	"add": func(a, b int) int { return a + b },
}
