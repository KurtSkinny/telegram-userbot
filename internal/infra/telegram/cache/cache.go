// Package cache — центральный кэш для быстрого разрешения tg.InputPeer* и доступа
// к метаданным диалогов Telegram. Пакет хранит пользователей, чаты, каналы и их
// служебные поля (username, title, флаги broadcast/megagroup), удаляя лишние RPC
// при обработке апдейтов. Источники данных:
//   - локальный кэш (карты ID → InputPeer* + мета);
//   - entities из апдейтов (tg.Entities);
//   - при необходимости — RPC-запрос users.getUsers только для пользователей.
// Также поддерживается предварительная загрузка всего списка диалогов
// (BuildPeerCache), что резко снижает холодные обращения к API.

package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	tgruntime "telegram-userbot/internal/infra/telegram/runtime"

	"github.com/gotd/td/tg"
)

// PeerCache хранит кэш InputPeer для всех типов диалогов и связанные метаданные.
// Потокобезопасность обеспечивается собственным RWMutex. Экземпляр используется
// как singleton: создаётся через Init(ctx, api) и доступен глобальными обёртками.
type PeerCache struct {
	ctx              context.Context
	api              *tg.Client
	mu               sync.RWMutex
	channels         map[int64]*tg.InputPeerChannel
	users            map[int64]*tg.InputPeerUser
	chats            map[int64]*tg.InputPeerChat
	channelUsernames map[int64]string
	userUsernames    map[int64]string
	userFirstNames   map[int64]string
	userLastNames    map[int64]string
	chatTitles       map[int64]string
	channelTitles    map[int64]string
	channelFlags     map[int64]channelFlags
	dialogs          []tg.DialogClass
}

// channelFlags отражает тип канала на стороне Telegram:
//   - broadcast  — «обычный» канал трансляции;
//   - megagroup  — супергруппа (исторически канал с особыми правами участников).
//
// Значения берутся из tg.Channel и кэшируются для быстрых проверок.
type channelFlags struct {
	broadcast bool
	megagroup bool
}

var (
	// peerCacheMu защищает глобальный singleton от гонок при инициализации/чтении.
	peerCacheMu       sync.RWMutex
	peerCacheInstance *PeerCache

	// errPeerCacheInitError сигнализирует о попытке создать кэш с пустыми аргументами.
	errPeerCacheInitError = errors.New("peercache initialization error; nil arguments")
	// errPeerCacheNotInitialized сообщает, что нужно вызвать Init до использования API пакета.
	errPeerCacheNotInitialized = errors.New("peercache: peer cache not initialized; call peercache.Init before use")
)

// Init инициализирует singleton кэша. Оба аргумента обязательны; при пустых
// значениях происходит panic(errPeerCacheInitError). Повторный вызов перезапишет
// предыдущий инстанс. Контекст используется для внутренних RPC при fallback.
func Init(ctx context.Context, api *tg.Client) {
	if ctx == nil || api == nil {
		panic(errPeerCacheInitError)
	}
	cache := &PeerCache{
		ctx:              ctx,
		api:              api,
		channels:         make(map[int64]*tg.InputPeerChannel),
		users:            make(map[int64]*tg.InputPeerUser),
		chats:            make(map[int64]*tg.InputPeerChat),
		channelUsernames: make(map[int64]string),
		userUsernames:    make(map[int64]string),
		userFirstNames:   make(map[int64]string),
		userLastNames:    make(map[int64]string),
		chatTitles:       make(map[int64]string),
		channelTitles:    make(map[int64]string),
		channelFlags:     make(map[int64]channelFlags),
	}
	peerCacheMu.Lock()
	peerCacheInstance = cache
	peerCacheMu.Unlock()
}

// mustPeerCache возвращает текущий singleton. Если Init не вызывали — паникует
// с errPeerCacheNotInitialized. Предназначена для внутренних/глобальных обёрток.
func mustPeerCache() *PeerCache {
	peerCacheMu.RLock()
	cache := peerCacheInstance
	peerCacheMu.RUnlock()
	if cache == nil {
		panic(errPeerCacheNotInitialized)
	}
	return cache
}

// getChannel безопасно возвращает сохраненный InputPeerChannel по ID.
// Метод использует RLock, чтобы позволить множеству одновременных читателей.
func (c *PeerCache) getChannel(id int64) (*tg.InputPeerChannel, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.channels[id]
	return p, ok
}

// getUser безопасно возвращает сохраненный InputPeerUser по ID.
func (c *PeerCache) getUser(id int64) (*tg.InputPeerUser, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	u, ok := c.users[id]
	return u, ok
}

// getChat безопасно возвращает сохраненный InputPeerChat по ID.
func (c *PeerCache) getChat(id int64) (*tg.InputPeerChat, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ch, ok := c.chats[id]
	return ch, ok
}

// putChannel сохраняет InputPeerChannel по ID. Ничего не делает при id==0 или nil-указателе.
func (c *PeerCache) putChannel(id int64, ch *tg.InputPeerChannel) {
	if id == 0 || ch == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channels[id] = ch
}

// putUser сохраняет InputPeerUser по ID. Ничего не делает при id==0 или nil-указателе.
func (c *PeerCache) putUser(id int64, u *tg.InputPeerUser) {
	if id == 0 || u == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.users[id] = u
}

// putChat сохраняет InputPeerChat по ID. Ничего не делает при id==0 или nil-указателе.
func (c *PeerCache) putChat(id int64, ch *tg.InputPeerChat) {
	if id == 0 || ch == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chats[id] = ch
}

// storeChannelMeta сохраняет username (без префикса @), заголовок и флаги канала.
// Используется при заполнении кэша из dialogs и при обработке entities.
func (c *PeerCache) storeChannelMeta(ch *tg.Channel) {
	if ch == nil {
		return
	}
	name := strings.TrimPrefix(ch.Username, "@")
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channelUsernames[ch.ID] = name
	c.channelTitles[ch.ID] = ch.Title
	c.channelFlags[ch.ID] = channelFlags{
		broadcast: ch.Broadcast,
		megagroup: ch.Megagroup,
	}
}

// storeUserMeta сохраняет username (без @), а также FirstName/LastName для быстрого доступа.
func (c *PeerCache) storeUserMeta(u *tg.User) {
	if u == nil {
		return
	}
	name := strings.TrimPrefix(u.Username, "@")
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userUsernames[u.ID] = name
	c.userFirstNames[u.ID] = u.FirstName
	c.userLastNames[u.ID] = u.LastName
}

// storeChatMeta сохраняет заголовок обычной группы (tg.Chat), у которой нет access_hash.
func (c *PeerCache) storeChatMeta(ch *tg.Chat) {
	if ch == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chatTitles[ch.ID] = ch.Title
}

// ChannelUsername возвращает username канала и флаг успеха. Пустые строки считаются «нет данных».
func (c *PeerCache) ChannelUsername(id int64) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	name, ok := c.channelUsernames[id]
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// ChannelUsername — потокобезопасный доступ к username канала через singleton.
func ChannelUsername(id int64) (string, bool) {
	return mustPeerCache().ChannelUsername(id)
}

// ChannelTitle возвращает заголовок канала и флаг успеха. Пустой заголовок трактуется как отсутствие.
func (c *PeerCache) ChannelTitle(id int64) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	title, ok := c.channelTitles[id]
	if !ok || title == "" {
		return "", false
	}
	return title, true
}

// ChannelTitle — глобальная обертка для получения заголовка канала из кэша.
func ChannelTitle(id int64) (string, bool) {
	return mustPeerCache().ChannelTitle(id)
}

// ChannelFlags возвращает (broadcast, megagroup, ok) для указанного канала.
func (c *PeerCache) ChannelFlags(id int64) (bool, bool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	flags, ok := c.channelFlags[id]
	if !ok {
		return false, false, false
	}
	return flags.broadcast, flags.megagroup, true
}

// ChannelFlags предоставляет доступ к флагам канала без прямой работы со структурой.
func ChannelFlags(id int64) (bool, bool, bool) {
	return mustPeerCache().ChannelFlags(id)
}

// UserUsername возвращает username пользователя и флаг успеха. Пустые значения отбрасываются.
func (c *PeerCache) UserUsername(id int64) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	name, ok := c.userUsernames[id]
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// UserUsername — глобальная обертка для доступа к username пользователя.
func UserUsername(id int64) (string, bool) {
	return mustPeerCache().UserUsername(id)
}

// UserFirstName возвращает имя пользователя и флаг успеха.
func (c *PeerCache) UserFirstName(id int64) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	name, ok := c.userFirstNames[id]
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// UserFirstName — глобальная обертка для доступа к имени пользователя.
func UserFirstName(id int64) (string, bool) {
	return mustPeerCache().UserFirstName(id)
}

// UserLastName возвращает фамилию пользователя и флаг успеха.
func (c *PeerCache) UserLastName(id int64) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	name, ok := c.userLastNames[id]
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// UserLastName — глобальная обертка для доступа к фамилии пользователя.
func UserLastName(id int64) (string, bool) {
	return mustPeerCache().UserLastName(id)
}

// ChatTitle возвращает заголовок обычной группы (tg.Chat) и флаг успеха.
func (c *PeerCache) ChatTitle(id int64) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	title, ok := c.chatTitles[id]
	if !ok || title == "" {
		return "", false
	}
	return title, true
}

// ChatTitle — глобальная обертка для получения заголовка чата.
func ChatTitle(id int64) (string, bool) {
	return mustPeerCache().ChatTitle(id)
}

// Dialogs возвращает копию списка диалогов. Возврат копии защищает внутренний кэш
// от случайной мутации вызывающим кодом.
func (c *PeerCache) Dialogs() []tg.DialogClass {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]tg.DialogClass, len(c.dialogs))
	copy(result, c.dialogs)
	return result
}

// Dialogs — глобальная обёртка, возвращающая копию диалогов из singleton-а.
func Dialogs() []tg.DialogClass {
	return mustPeerCache().Dialogs()
}

// GetInputPeerUser возвращает tg.InputPeerUser для userID. Сначала пытается
// извлечь из кэша/entities, при необходимости делает fallback users.getUsers.
func (c *PeerCache) GetInputPeerUser(uid int64) (*tg.InputPeerUser, error) {
	peer, err := c.GetInputPeerRaw(tg.Entities{Users: nil}, &tg.Message{PeerID: &tg.PeerUser{UserID: uid}})
	if err != nil {
		return nil, err
	}
	u, ok := peer.(*tg.InputPeerUser)
	if !ok {
		return nil, fmt.Errorf("unexpected type for peer: %T", peer)
	}
	return u, nil
}

// GetInputPeerUser — глобальная обёртка, обращающаяся к singleton-кэшу.
func GetInputPeerUser(uid int64) (*tg.InputPeerUser, error) {
	return mustPeerCache().GetInputPeerUser(uid)
}

// GetInputPeerByKind возвращает tg.InputPeerClass по строковому типу: "user"|"chat"|"channel".
// Это тонкая обёртка вокруг универсального GetInputPeerRaw.
func (c *PeerCache) GetInputPeerByKind(class string, id int64) (tg.InputPeerClass, error) {
	entities := tg.Entities{}
	msg := &tg.Message{}

	switch class {
	case "user":
		msg = &tg.Message{PeerID: &tg.PeerUser{UserID: id}}
	case "chat":
		msg = &tg.Message{PeerID: &tg.PeerChat{ChatID: id}}
	case "channel":
		msg = &tg.Message{PeerID: &tg.PeerChannel{ChannelID: id}}
	}

	return c.GetInputPeerRaw(entities, msg)
}

// GetInputPeerByKind — глобальная обёртка для singleton-а.
func GetInputPeerByKind(class string, id int64) (tg.InputPeerClass, error) {
	return mustPeerCache().GetInputPeerByKind(class, id)
}

// GetInputPeerRaw подбирает корректный tg.InputPeerClass по message.PeerID и entities.
// Порядок разрешения:
//  1. поиск в локальном кэше (быстрее всего);
//  2. разбор entities из апдейта с сохранением метаданных в кэш;
//  3. fallback-RPC только для пользователей (users.getUsers). Для чатов/каналов
//     без entities получить access_hash напрямую нельзя — возвращаем ошибку.
//
// При успехе соответствующие метаданные сохраняются в кэш.
func (c *PeerCache) GetInputPeerRaw(
	entities tg.Entities,
	msg *tg.Message,
) (tg.InputPeerClass, error) {
	if msg == nil {
		return nil, errors.New("message is nil")
	}

	switch peer := msg.PeerID.(type) {
	// ---------- Пользователь: кэш → entities → fallback RPC ----------
	case *tg.PeerUser:
		if p, ok := c.getUser(peer.UserID); ok {
			return p, nil
		}
		if user, ok := entities.Users[peer.UserID]; ok && user != nil {
			p := &tg.InputPeerUser{
				UserID:     user.ID,
				AccessHash: user.AccessHash,
			}
			c.putUser(user.ID, p)
			c.storeUserMeta(user)
			return p, nil
		}
		// fallback: запрос напрямую
		return c.getUserFallback(peer.UserID)

		// У обычных групп (tg.Chat) нет access_hash. Если записи нет ни в локальном кэше,
		// ни в entities, выполнить прямой RPC для получения InputPeerChat невозможно.
	case *tg.PeerChat:
		if p, ok := c.getChat(peer.ChatID); ok {
			return p, nil
		}
		if chat, ok := entities.Chats[peer.ChatID]; ok && chat != nil {
			p := &tg.InputPeerChat{ChatID: chat.ID}
			c.putChat(chat.ID, p)
			c.storeChatMeta(chat)
			return p, nil
		}
		return nil, fmt.Errorf("chat %d not found in cache or entities", peer.ChatID)

	// ---------- Канал/супергруппа: кэш → entities; прямого fallback нет ----------
	case *tg.PeerChannel:
		if p, ok := c.getChannel(peer.ChannelID); ok {
			return p, nil
		}
		if ch, ok := entities.Channels[peer.ChannelID]; ok && ch != nil {
			p := &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}
			c.putChannel(ch.ID, p)
			c.storeChannelMeta(ch)
			return p, nil
		}
		return nil, fmt.Errorf("channel %d not found in cache or entities", peer.ChannelID)

	default:
		return nil, fmt.Errorf("unsupported PeerID type: %T", peer)
	}
}

// GetInputPeerRaw — глобальная обёртка над методом кэша.
func GetInputPeerRaw(entities tg.Entities, msg *tg.Message) (tg.InputPeerClass, error) {
	return mustPeerCache().GetInputPeerRaw(entities, msg)
}

// getUserFallback выполняет RPC users.getUsers, когда данных нет ни в кэше, ни в entities.
// Достаточно передать InputUser с user_id и нулевым access_hash — сервер вернёт актуальные данные.
func (c *PeerCache) getUserFallback(userID int64) (*tg.InputPeerUser, error) {
	// fallback: запрос напрямую
	users, err := c.api.UsersGetUsers(c.ctx, []tg.InputUserClass{
		&tg.InputUser{UserID: userID, AccessHash: 0},
	})
	if err != nil {
		return nil, fmt.Errorf("UsersGetUsers failed: %w", err)
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("user %d not found", userID)
	}
	if u, ok := users[0].(*tg.User); ok {
		p := &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash}
		c.putUser(u.ID, p)
		c.storeUserMeta(u)
		return p, nil
	}
	return nil, fmt.Errorf("unexpected type for user %d", userID)
}

// getDialogs постранично получает список диалогов. Использует пагинацию по трём
// параметрам (offset_date, offset_id, offset_peer) и формирует offset_peer на
// основе заранее накопленных access_hash. Между вызовами делаются небольшие
// случайные задержки, чтобы не выглядело как спам, и чтобы уважать лимиты API.
func (c *PeerCache) getDialogs() (*tg.MessagesDialogs, error) {
	const (
		waitMinMs = 500
		waitMaxMs = 1500
		limit     = 100
	)
	result := &tg.MessagesDialogs{}

	offsetDate := 0
	offsetID := 0
	offsetPeer := tg.InputPeerClass(&tg.InputPeerEmpty{})

	userHashes := make(map[int64]int64, limit)
	channelHashes := make(map[int64]int64, limit)

	// Небольшая рандомная пауза перед первым запросом, чтобы не стартовать холодным бурстом.
	tgruntime.WaitRandomTimeMs(c.ctx, waitMinMs, waitMaxMs)

	for {
		resp, err := c.api.MessagesGetDialogs(c.ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate,
			OffsetID:   offsetID,
			OffsetPeer: offsetPeer,
			Limit:      limit,
		})
		if err != nil {
			return nil, fmt.Errorf("MessagesGetDialogs: %w", err)
		}

		// Приводим ответ к каноническому виду (MessagesDialogs), чтобы унифицировать парсинг.
		batch, err := normalizeDialogs(resp)
		if err != nil {
			return nil, err
		}

		if len(batch.Dialogs) == 0 {
			break
		}

		result.Dialogs = append(result.Dialogs, batch.Dialogs...)
		result.Messages = append(result.Messages, batch.Messages...)
		result.Chats = append(result.Chats, batch.Chats...)
		result.Users = append(result.Users, batch.Users...)

		// Сохраняем access_hash в локальные карты для корректного построения следующего offset_peer.
		updateHashesFromBatch(batch, userHashes, channelHashes)

		lastDialog := batch.Dialogs[len(batch.Dialogs)-1]
		// Сохраняем предыдущие оффсеты на случай, если Telegram вернёт нули.
		prevOffsetDate := offsetDate
		prevOffsetID := offsetID

		switch d := lastDialog.(type) {
		case *tg.Dialog:
			offsetID = d.TopMessage
			offsetDate = messageDate(batch.Messages, d.TopMessage)
			offsetPeer = dialogPeerToInput(d.Peer, userHashes, channelHashes)
		case *tg.DialogFolder:
			offsetID = d.TopMessage
			offsetDate = messageDate(batch.Messages, d.TopMessage)
			offsetPeer = dialogPeerToInput(d.Peer, userHashes, channelHashes)
		default:
			break
		}

		// Страховка от нулевых оффсетов: возвращаемся к предыдущим значениям.
		if offsetDate == 0 {
			offsetDate = prevOffsetDate
		}
		if offsetID == 0 {
			offsetID = prevOffsetID
		}
		if offsetPeer == nil {
			offsetPeer = &tg.InputPeerEmpty{}
		}

		if len(batch.Dialogs) < limit {
			break
		}

		// Пауза между страницами, чтобы не упираться в лимиты и выглядело «по‑человечески».
		tgruntime.WaitRandomTimeMs(c.ctx, waitMinMs, waitMaxMs)
	}

	return result, nil
}

// updateHashesFromBatch переносит access_hash пользователей и каналов из пачки
// dialogs в локальные карты. Нужен для корректного построения offset_peer.
func updateHashesFromBatch(batch *tg.MessagesDialogs, userHashes, channelHashes map[int64]int64) {
	for _, u := range batch.Users {
		if user, ok := u.(*tg.User); ok {
			userHashes[user.ID] = user.AccessHash
		}
	}
	for _, ch := range batch.Chats {
		if channel, ok := ch.(*tg.Channel); ok {
			channelHashes[channel.ID] = channel.AccessHash
		}
	}
}

// BuildPeerCache загружает диалоги пользователя и предзаполняет карты users/chats/channels
// и метаданные. Это резко сокращает количество RPC при холодном старте.
func (c *PeerCache) BuildPeerCache() error {
	dialogs, err := c.getDialogs()
	if err != nil {
		return err
	}

	for _, chat := range dialogs.Chats {
		switch v := chat.(type) {
		case *tg.Channel:
			c.putChannel(v.ID, &tg.InputPeerChannel{ChannelID: v.ID, AccessHash: v.AccessHash})
			c.storeChannelMeta(v)
		case *tg.Chat:
			c.putChat(v.ID, &tg.InputPeerChat{ChatID: v.ID})
			c.storeChatMeta(v)
		}
	}

	for _, user := range dialogs.Users {
		if u, ok := user.(*tg.User); ok {
			c.putUser(u.ID, &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash})
			c.storeUserMeta(u)
		}
	}

	c.mu.Lock()
	c.dialogs = make([]tg.DialogClass, len(dialogs.Dialogs))
	copy(c.dialogs, dialogs.Dialogs)
	c.mu.Unlock()

	return nil
}

// BuildPeerCache — глобальная обёртка для заполнения singleton-а данными о диалогах.
func BuildPeerCache() error {
	return mustPeerCache().BuildPeerCache()
}

// normalizeDialogs приводит разные варианты ответа к единому виду. Для
// MessagesDialogsNotModified возвращает ошибку, чтобы вызывающий мог решать,
// что делать с «нет изменений».
func normalizeDialogs(resp tg.MessagesDialogsClass) (*tg.MessagesDialogs, error) {
	switch d := resp.(type) {
	case *tg.MessagesDialogs:
		return d, nil
	case *tg.MessagesDialogsSlice:
		return &tg.MessagesDialogs{
			Dialogs:  d.Dialogs,
			Messages: d.Messages,
			Chats:    d.Chats,
			Users:    d.Users,
		}, nil
	case *tg.MessagesDialogsNotModified:
		return nil, errors.New("dialogs not modified")
	default:
		return nil, fmt.Errorf("unexpected dialogs response: %T", resp)
	}
}

// messageDate ищет сообщение по ID и возвращает его дату (unix). Работает как
// для обычных сообщений, так и для сервисных.
func messageDate(messages []tg.MessageClass, id int) int {
	for _, msg := range messages {
		switch m := msg.(type) {
		case *tg.Message:
			if m.ID == id {
				return m.Date
			}
		case *tg.MessageService:
			if m.ID == id {
				return m.Date
			}
		}
	}
	return 0
}

// dialogPeerToInput формирует tg.InputPeerClass для пагинации по dialogs, опираясь
// на заранее собранные access_hash. Для неизвестных типов возвращает InputPeerEmpty.
func dialogPeerToInput(peer tg.PeerClass, userHashes, channelHashes map[int64]int64) tg.InputPeerClass {
	switch p := peer.(type) {
	case *tg.PeerUser:
		return &tg.InputPeerUser{UserID: p.UserID, AccessHash: userHashes[p.UserID]}
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: p.ChatID}
	case *tg.PeerChannel:
		return &tg.InputPeerChannel{ChannelID: p.ChannelID, AccessHash: channelHashes[p.ChannelID]}
	default:
		return &tg.InputPeerEmpty{}
	}
}
