// Package core содержит оболочки вокруг gotd для авторизации и управления сессией пользовательского Telegram‑клиента.
// Этот файл описывает клиентское ядро (ClientCore): создание клиента, интерактивную авторизацию,
// доступ к RPC и корректное завершение сессии с очисткой локального состояния.

package core

// // ClientCore — тонкая обёртка над gotd, объединяющая сетевой клиент и RPC‑клиента.
// // Хранит снимок конфигурации окружения, необходимый для интерактивной авторизации и управления сессией.
// type ClientCore struct {
// 	Client *telegram.Client // Сетевой клиент gotd: держит MTProto‑соединение, прокачивает апдейты, управляет сессией
// 	API    *tg.Client       // Тонкий RPC‑клиент для вызовов Telegram (Auth, Messages, Users и т.д.)
// }

// // New создаёт ClientCore и инициализирует gotd‑клиент на основе текущего Env.
// // dispatcher передан для совместимости с вызывающим кодом, сам New не назначает его —
// // ожидается, что UpdateHandler уже указан в options.UpdateHandler.
// // Важно: поле cfg в возвращаемом ClientCore должно быть заполнено (например, через присваивание
// // config.Env()) до вызовов Login/Logout, иначе интерактивная авторизация и очистка сессии
// // не смогут использовать номер телефона и путь к файлу сессии.
// func New(options telegram.Options) *telegram.Client {
// 	// Создаём сетевой клиент gotd, используя API ID/Hash из Env и переданные options.
// 	return telegram.NewClient(config.Env().APIID, config.Env().APIHash, options)

// 	// return &ClientCore{
// 	// 	Client: client,
// 	// 	API:    client.API(),
// 	// }
// 	// NOTE: cfg не инициализируется здесь. Заполните c.cfg снаружи (например, c.cfg = config.Env()).
// }

// // Login выполняет интерактивную авторизацию:
// //  1. проверяет текущий статус сессии (Auth.Status),
// //  2. если не авторизованы — запускает auth.Flow с TerminalAuthenticator,
// //  3. при необходимости обрабатывает ввод кода/2FA и приём условий использования.
// func (c *ClientCore) Login(ctx context.Context) error {
// 	// 1) Быстрая проверка: есть ли валидная сессия.
// 	status, err := c.Client.Auth().Status(ctx)
// 	if err != nil {
// 		return fmt.Errorf("auth status error: %w", err)
// 	}

// 	// Сессия валидна — продолжаем без интерактива.
// 	if status.Authorized {
// 		logger.Debug("Already authorized, session restored")
// 		return nil
// 	}

// 	// 2) Готовим интерактивный сценарий
// 	flow := auth.NewFlow(
// 		TerminalAuthenticator{PhoneNumber: config.Env().PhoneNumber},
// 		auth.SendCodeOptions{},
// 	)

// 	return c.Client.Auth().IfNecessary(ctx, flow)
// }

// func (c *ClientCore) Logout(ctx context.Context) error {
// 	if _, err := c.API.AuthLogOut(ctx); err != nil {
// 		return fmt.Errorf("logout failed: %w", err)
// 	}
// 	if err := os.Remove(config.Env().SessionFile); err != nil && !os.IsNotExist(err) {
// 		return fmt.Errorf("failed to remove session file: %w", err)
// 	}
// 	logger.Info("Logged out successfully")
// 	return nil
// }
