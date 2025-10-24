// Package core содержит оболочки вокруг gotd для авторизации и управления сессией пользовательского Telegram‑клиента.
// Этот файл описывает клиентское ядро (ClientCore): создание клиента, интерактивную авторизацию,
// доступ к RPC и корректное завершение сессии с очисткой локального состояния.

package core

import (
	"context"
	"fmt"
	"os"

	"telegram-userbot/internal/infra/config"
	"telegram-userbot/internal/infra/logger"
	"telegram-userbot/internal/infra/pr"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
)

// ClientCore — тонкая обёртка над gotd, объединяющая сетевой клиент и RPC‑клиента.
// Хранит снимок конфигурации окружения, необходимый для интерактивной авторизации и управления сессией.
type ClientCore struct {
	Client *telegram.Client // Сетевой клиент gotd: держит MTProto‑соединение, прокачивает апдейты, управляет сессией
	API    *tg.Client       // Тонкий RPC‑клиент для вызовов Telegram (Auth, Messages, Users и т.д.)
	cfg    config.EnvConfig // Снимок конфигурации (API‑параметры, номер телефона, путь к сессии). ДОЛЖЕН быть установлен.
}

// New создаёт ClientCore и инициализирует gotd‑клиент на основе текущего Env.
// dispatcher передан для совместимости с вызывающим кодом, сам New не назначает его —
// ожидается, что UpdateHandler уже указан в options.UpdateHandler.
// Важно: поле cfg в возвращаемом ClientCore должно быть заполнено (например, через присваивание
// config.Env()) до вызовов Login/Logout, иначе интерактивная авторизация и очистка сессии
// не смогут использовать номер телефона и путь к файлу сессии.
func New(dispatcher telegram.UpdateHandler, options telegram.Options) (*ClientCore, error) {
	// Создаём сетевой клиент gotd, используя API ID/Hash из Env и переданные options.
	client := telegram.NewClient(config.Env().APIID, config.Env().APIHash, options)

	return &ClientCore{
		Client: client,
		API:    client.API(),
	}, nil
	// NOTE: cfg не инициализируется здесь. Заполните c.cfg снаружи (например, c.cfg = config.Env()).
}

// Login выполняет интерактивную авторизацию:
//  1. проверяет текущий статус сессии (Auth.Status),
//  2. если не авторизованы — запускает auth.Flow с TerminalAuthenticator,
//  3. при необходимости обрабатывает ввод кода/2FA и приём условий использования.
//
// Требования: c.cfg.PhoneNumber должен быть установлен. Возвращает ошибку сети/авторизации.
func (c *ClientCore) Login(ctx context.Context) error {
	// 1) Быстрая проверка: есть ли валидная сессия.
	status, err := c.Client.Auth().Status(ctx)
	if err != nil {
		return fmt.Errorf("auth status error: %w", err)
	}

	// Сессия валидна — продолжаем без интерактива.
	if status.Authorized {
		logger.Debug("Already authorized, session restored")
		return nil
	}

	// 2) Готовим интерактивный сценарий: TerminalAuthenticator читает номер из c.cfg.PhoneNumber.
	flow := auth.NewFlow(
		TerminalAuthenticator{PhoneNumber: c.cfg.PhoneNumber},
		auth.SendCodeOptions{},
	)

	// 3) Запускаем авторизацию при необходимости. Включает отправку кода/2FA/TOS.
	return c.Client.Auth().IfNecessary(ctx, flow)
}

// Logout завершает серверную сессию и очищает локальный файл сессии.
// Требования: c.cfg.SessionFile должен указывать на путь к файлу сессии. Игнорирует отсутствие файла.
func (c *ClientCore) Logout(ctx context.Context) error {
	// 1) Закрываем серверную сессию через RPC.
	if _, err := c.API.AuthLogOut(ctx); err != nil {
		return fmt.Errorf("logout failed: %w", err)
	}
	// 2) Удаляем локальный файл сессии; отсутствие файла не считается ошибкой.
	if err := os.Remove(c.cfg.SessionFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove session file: %w", err)
	}
	// 3) Сообщаем пользователю об успешном выходе.
	pr.Println("Logged out successfully")
	return nil
}
