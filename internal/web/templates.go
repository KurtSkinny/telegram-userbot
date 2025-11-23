package web

// layoutTemplate - базовый layout с навигацией
const layoutTemplate = `{{define "layout"}}
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{{.Title}} - Telegram Userbot</title>
    <script src="https://cdn.tailwindcss.com"></script>
    <script src="https://unpkg.com/htmx.org@1.9.10"></script>
</head>
<body class="bg-gray-50">
    <!-- Navigation -->
    <nav class="bg-white shadow-lg">
        <div class="max-w-7xl mx-auto px-4">
            <div class="flex justify-between h-16">
                <div class="flex space-x-8">
                    <div class="flex items-center">
                        <span class="text-xl font-bold text-blue-600">📱 Userbot Web UI</span>
                    </div>
                    <div class="hidden md:flex items-center space-x-4">
                        <a href="/" class="px-3 py-2 rounded-md text-sm font-medium {{if eq .Page "dashboard"}}bg-blue-100 text-blue-700{{else}}text-gray-700 hover:bg-gray-100{{end}}">Status</a>
                        <a href="/filters" class="px-3 py-2 rounded-md text-sm font-medium {{if eq .Page "filters"}}bg-blue-100 text-blue-700{{else}}text-gray-700 hover:bg-gray-100{{end}}">Filters</a>
                        <a href="/recipients" class="px-3 py-2 rounded-md text-sm font-medium {{if eq .Page "recipients"}}bg-blue-100 text-blue-700{{else}}text-gray-700 hover:bg-gray-100{{end}}">Recipients</a>
                        <a href="/logs" class="px-3 py-2 rounded-md text-sm font-medium {{if eq .Page "logs"}}bg-blue-100 text-blue-700{{else}}text-gray-700 hover:bg-gray-100{{end}}">Logs</a>
                    </div>
                </div>
            </div>
        </div>
    </nav>

    <!-- Main Content -->
    <main class="max-w-7xl mx-auto px-4 py-6">
        {{if eq .Page "dashboard"}}{{template "dashboard" .}}{{end}}
        {{if eq .Page "logs"}}{{template "logs" .}}{{end}}
        {{if eq .Page "filters"}}{{template "filters" .}}{{end}}
        {{if eq .Page "recipients"}}{{template "recipients" .}}{{end}}
    </main>
</body>
</html>
{{end}}`

// dashboardTemplate - главная страница (Status)
const dashboardTemplate = `{{define "dashboard"}}
<div class="space-y-6">
    <h1 class="text-3xl font-bold text-gray-900">Dashboard</h1>
    
    <!-- Grid 2x2 -->
    <div class="grid grid-cols-1 md:grid-cols-2 gap-6">
        <!-- Status Panel -->
        <div class="bg-white rounded-lg shadow-md p-6">
            <h2 class="text-xl font-bold mb-4 flex items-center gap-2">
                <span>📊</span>
                <span>Queue Status</span>
            </h2>
            <div id="status-panel" class="space-y-2">
                <p class="text-gray-500">Loading...</p>
            </div>
            <button 
                hx-post="/api/flush" 
                hx-target="#status-panel" 
                hx-swap="innerHTML"
                class="mt-4 w-full bg-blue-600 hover:bg-blue-700 text-white px-4 py-2 rounded transition">
                Flush Queue
            </button>
        </div>

        <!-- Dialogs Panel -->
        <div class="bg-white rounded-lg shadow-md p-6">
            <h2 class="text-xl font-bold mb-4 flex items-center gap-2">
                <span>💬</span>
                <span>Dialogs</span>
            </h2>
            <div id="dialogs-panel" class="space-y-2 max-h-64 overflow-y-auto">
                <p class="text-gray-500">Loading...</p>
            </div>
            <button 
                hx-post="/api/refresh" 
                hx-target="#dialogs-panel" 
                hx-swap="innerHTML"
                class="mt-4 w-full bg-green-600 hover:bg-green-700 text-white px-4 py-2 rounded transition">
                Refresh Dialogs
            </button>
        </div>

        <!-- Filters Panel -->
        <div class="bg-white rounded-lg shadow-md p-6">
            <h2 class="text-xl font-bold mb-4 flex items-center gap-2">
                <span>🔍</span>
                <span>Filters</span>
            </h2>
            <div id="filters-panel" class="space-y-2">
                <p class="text-gray-600">Filters loaded and active</p>
                <p class="text-sm text-gray-500">Configure filters in <code class="bg-gray-100 px-1">assets/filters.json</code></p>
            </div>
            <button 
                hx-post="/api/reload" 
                hx-target="#filters-panel" 
                hx-swap="innerHTML"
                class="mt-4 w-full bg-purple-600 hover:bg-purple-700 text-white px-4 py-2 rounded transition">
                Reload Filters
            </button>
        </div>

        <!-- System Info Panel -->
        <div class="bg-white rounded-lg shadow-md p-6">
            <h2 class="text-xl font-bold mb-4 flex items-center gap-2">
                <span>⚙️</span>
                <span>System Info</span>
            </h2>
            <div id="system-panel" class="space-y-2">
                <p class="text-gray-500">Loading...</p>
            </div>
            <div id="system-panel-test" class="space-y-2">
            </div>
            <button 
                hx-post="/api/test" 
                hx-target="#system-panel-test" 
                hx-swap="innerHTML"
                class="mt-4 w-full bg-orange-600 hover:bg-orange-700 text-white px-4 py-2 rounded transition">
                Test Connection
            </button>
        </div>
    </div>
</div>

<script>
    // Загружаем данные при загрузке страницы
    document.addEventListener('DOMContentLoaded', function() {
        htmx.ajax('GET', '/api/status', {target: '#status-panel', swap: 'innerHTML'});
        htmx.ajax('GET', '/api/list', {target: '#dialogs-panel', swap: 'innerHTML'});
        
        // Загружаем системную информацию
        Promise.all([
            fetch('/api/whoami').then(r => r.text()),
            fetch('/api/version').then(r => r.text())
        ]).then(([whoami, version]) => {
            document.getElementById('system-panel').innerHTML = whoami + version;
        });
    });
</script>
{{end}}`

// logsTemplate - страница просмотра логов
const logsTemplate = `{{define "logs"}}
<div class="space-y-6">
    <h1 class="text-3xl font-bold text-gray-900">Logs</h1>
    
    <div class="bg-white rounded-lg shadow-md p-6">
        <div id="logs-container" class="font-mono text-sm space-y-1">
            <p class="text-gray-500">Loading logs...</p>
        </div>
        
        <!-- Pagination -->
        <div id="pagination" class="mt-6 flex justify-center">
        </div>
    </div>
</div>

<script>
    document.addEventListener('DOMContentLoaded', function() {
        htmx.ajax('GET', '/api/logs?page=1', {target: '#logs-container', swap: 'innerHTML'});
    });
</script>
{{end}}`

// filtersTemplate - заглушка для управления фильтрами
const filtersTemplate = `{{define "filters"}}
<div class="space-y-6">
    <h1 class="text-3xl font-bold text-gray-900">Filter Management</h1>
    
    <div class="bg-white rounded-lg shadow-md p-8">
        <div class="text-center py-12">
            <div class="text-6xl mb-4">🚧</div>
            <h2 class="text-2xl font-bold text-gray-800 mb-2">Filter Management</h2>
            <p class="text-gray-600 mb-6">This feature is under development.</p>
            
            <div class="max-w-md mx-auto text-left bg-blue-50 rounded-lg p-6">
                <p class="font-semibold text-blue-900 mb-3">Future capabilities:</p>
                <ul class="space-y-2 text-blue-800">
                    <li>• View and edit filter rules</li>
                    <li>• Test filters against messages</li>
                    <li>• Debug filter matching</li>
                    <li>• Import/Export filters</li>
                </ul>
            </div>
            
            <p class="text-gray-500 mt-6">Stay tuned! 🚀</p>
        </div>
    </div>
</div>
{{end}}`

// recipientsTemplate - заглушка для управления получателями
const recipientsTemplate = `{{define "recipients"}}
<div class="space-y-6">
    <h1 class="text-3xl font-bold text-gray-900">Recipients Management</h1>
    
    <div class="bg-white rounded-lg shadow-md p-8">
        <div class="text-center py-12">
            <div class="text-6xl mb-4">🚧</div>
            <h2 class="text-2xl font-bold text-gray-800 mb-2">Recipients Management</h2>
            <p class="text-gray-600 mb-6">This feature is under development.</p>
            
            <div class="max-w-md mx-auto text-left bg-green-50 rounded-lg p-6">
                <p class="font-semibold text-green-900 mb-3">Future capabilities:</p>
                <ul class="space-y-2 text-green-800">
                    <li>• Add/Remove recipients</li>
                    <li>• Edit recipient settings</li>
                    <li>• Test recipient notifications</li>
                    <li>• Manage recipient groups</li>
                </ul>
            </div>
            
            <p class="text-gray-500 mt-6">Stay tuned! 🚀</p>
        </div>
    </div>
</div>
{{end}}`
