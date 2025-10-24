// Package debug — вспомогательные утилиты для отладки юзербота.
// Здесь сосредоточены функции печати входящих событий и тонкая обёртка над
// структурированным логированием. Цели:
//   - быстро просматривать апдейты в консоли (с нормальными именами авторов/чатов);
//   - писать структурные записи в общий лог только при активном DEBUG;
//   - минимизировать шум и резать слишком длинные тексты по границе рун.
// Пакет не влияет на бизнес‑логику и может быть выключен в проде переключателем DEBUG.

package debug

import (
	"fmt"
	"strings"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/pr"
	"telegram-userbot/internal/infra/telegram/cache"
	"unicode/utf8"

	"github.com/gotd/td/tg"
	"go.uber.org/zap"
)

// DEBUG — глобальный переключатель режима отладки. Когда false, все функции пакета
// молчат. Переменная нечитаема из конфигурации автоматически: предполагается,
// что прод‑сборка запускается с DEBUG=false.
var DEBUG = true

// PrintUpdate печатает компактное представление входящего сообщения в консоль.
// Формат: [prefix] <источник> > <читаемое имя>: <обрезанный текст>.
// Особенности:
//   - текст режется до безопасной длины по рунам, чтобы не ломать UTF‑8;
//   - для пользователей/чатов/каналов имена берутся из cache.*;
//   - entities передаются для потенциальных улучшений (сейчас не используются);
//   - отсутствующие метаданные заменяются плейсхолдерами ("<unknown>").
func PrintUpdate(prefix string, msg *tg.Message, entities tg.Entities) {
	if !DEBUG {
		// Отладка выключена — ничего не делаем. Этот ранний выход упраздняет лишнюю работу.
		return
	}
	var from string
	var name string
	text := msg.Message

	// Ограничиваем размер вывода, чтобы не раздувать консоль длинными сообщениями.
	const textMaxLen = 50

	// Считаем и обрезаем по рунам, а не по байтам, чтобы не порвать Unicode‑символы.
	// Специально используем слайс рун, а не substring по байтам.
	if utf8.RuneCountInString(text) > textMaxLen {
		runes := []rune(text)
		text = string(runes[:textMaxLen]) + "..."
	}

	// Определяем тип собеседника/чата и вытягиваем читабельные имена из кэша.
	switch peer := msg.PeerID.(type) {
	case *tg.PeerUser:
		first, _ := cache.UserFirstName(peer.UserID)
		last, _ := cache.UserLastName(peer.UserID)
		username, _ := cache.UserUsername(peer.UserID)
		// Формируем полное имя. Если оно пустое, используем безопасный плейсхолдер.
		fullname := strings.TrimSpace(first + " " + last)
		if fullname == "" {
			fullname = "<unknown>"
		}
		from = "User"
		name = fmt.Sprintf("'%s' (@%s)", fullname, username)
	case *tg.PeerChat:
		title, _ := cache.ChatTitle(peer.ChatID)
		if title == "" {
			title = "<unknown chat>"
		}
		from = "Chat"
		name = fmt.Sprintf("'%s'", title)

	case *tg.PeerChannel:
		title, _ := cache.ChannelTitle(peer.ChannelID)
		username, _ := cache.ChannelUsername(peer.ChannelID)
		broadcast, megagroup, _ := cache.ChannelFlags(peer.ChannelID)
		// У каналов/супергрупп различаем два режима для наглядного лейбла.
		label := "Channel-like"
		if broadcast {
			label = "Channel"
		} else if megagroup {
			label = "Supergroup"
		}
		if title == "" {
			title = "<untitled channel>"
		}
		from = label
		name = fmt.Sprintf("'%s' (@%s)", title, username)
	default:
		// На случай редких/новых типов peer — печатаем отладочную форму.
		from = "Unknown"
		name = fmt.Sprintf("%+v", peer)
	}

	// Финальный вывод одной строки: префикс, тип отправителя, имя и урезанный текст.
	pr.Printf("[%s] %s > %s: %s\n", prefix, from, name, text)
}

// Debug пишет запись уровня Debug в общий лог только при активном DEBUG.
// Поля передаются как zap.Field для структурированного вывода.
func Debug(msg string, fields ...zap.Field) {
	if DEBUG {
		logger.Logger().Debug(msg, fields...)
	}
}

// Info пишет информационную запись при активном DEBUG. Поля — произвольные.
func Info(msg string, fields ...zap.Field) {
	if DEBUG {
		logger.Logger().Info(msg, fields...)
	}
}

// Warn пишет предупреждение в лог, если DEBUG=true.
func Warn(msg string, fields ...zap.Field) {
	if DEBUG {
		logger.Logger().Warn(msg, fields...)
	}
}

// Error пишет ошибку в лог при активном DEBUG, не паникует и не прерывает выполнение.
func Error(msg string, fields ...zap.Field) {
	if DEBUG {
		logger.Logger().Error(msg, fields...)
	}
}
