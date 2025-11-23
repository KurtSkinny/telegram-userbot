package web

import (
	"net/http"

	"telegram-userbot/internal/logger"
)

const (
	sessionCookieName = "userbot_session"
	sessionMaxAge     = 3600 // 1 час в секундах
)

// authMiddleware проверяет аутентификацию пользователя
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Проверяем токен из query параметра (для первичной авторизации)
		token := r.URL.Query().Get("token")
		if token != "" {
			sessionID, valid := s.auth.ValidateToken(token)
			if valid {
				// Устанавливаем cookie с session ID
				http.SetCookie(w, &http.Cookie{
					Name:     sessionCookieName,
					Value:    sessionID,
					Path:     "/",
					MaxAge:   sessionMaxAge,
					HttpOnly: true,
					SameSite: http.SameSiteStrictMode,
				})
				// Удаляем активный токен после успешного использования
				s.auth.DeleteCurrentToken()
				// Редирект на главную без токена в URL
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			// Невалидный токен
			logger.Warn("Invalid auth token attempt")
			http.Error(w, "Invalid authentication token", http.StatusUnauthorized)
			return
		}

		// Проверяем существующую сессию через cookie
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			// Нет cookie - показываем страницу с ошибкой
			s.renderUnauthorized(w, r)
			return
		}

		// Валидируем сессию
		if !s.auth.ValidateSession(cookie.Value) {
			// Сессия истекла или невалидна
			logger.Debug("Session expired or invalid")
			s.renderUnauthorized(w, r)
			return
		}

		// Обновляем cookie
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    cookie.Value,
			Path:     "/",
			MaxAge:   sessionMaxAge,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})

		// Сессия валидна, пропускаем дальше
		next.ServeHTTP(w, r)
	})
}

// renderUnauthorized отображает страницу с сообщением об отсутствии авторизации
func (s *Server) renderUnauthorized(w http.ResponseWriter, r *http.Request) {
	_ = r // reserved for future use
	logger.Debugf("Unauthorized access: %s %s from %s",
		r.Method, r.URL.Path, r.RemoteAddr)
	w.WriteHeader(http.StatusUnauthorized)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	html := `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Authentication Required - Telegram Userbot</title>
    <script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="bg-gray-100">
    <div class="min-h-screen flex items-center justify-center">
        <div class="bg-white rounded-lg shadow-lg p-8 max-w-md w-full">
            <div class="text-center">
                <svg class="mx-auto h-12 w-12 text-red-500" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 15v2m-6 4h12a2 2 0 002-2v-6a2 2 0 00-2-2H6a2 2 0 00-2 2v6a2 2 0 002 2zm10-10V7a4 4 0 00-8 0v4h8z"></path>
                </svg>
                <h1 class="mt-4 text-2xl font-bold text-gray-900">Authentication Required</h1>
                <p class="mt-2 text-gray-600">You need to authenticate to access this page.</p>
                <div class="mt-6 p-4 bg-blue-50 rounded-lg">
                    <p class="text-sm text-blue-800">
                        <strong>How to authenticate:</strong><br>
                        Send <code class="bg-blue-100 px-2 py-1 rounded">auth</code> command to your userbot in Telegram.
                        You will receive a link to access the web interface.
                    </p>
                </div>
            </div>
        </div>
    </div>
</body>
</html>`

	writeResponse(w, []byte(html))
}

// loggingMiddleware логирует все запросы
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Debugf("HTTP %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}
