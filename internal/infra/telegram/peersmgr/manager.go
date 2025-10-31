// Package peersmgr — обёртка над gotd peers.Manager с персистентным хранилищем на bbolt.
// Сервис отвечает за:
//   - открытие/закрытие базы данных кэша пиров;
//   - подготовку менеджера пиров (в памяти) и доступ к нему;
//   - загрузку сохранённых peers из файла в менеджер при старте;
//   - хранение снимка диалогов, доступного офлайн (CLI list).
package peersmgr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bboltdb "github.com/gotd/contrib/bbolt"
	contribstorage "github.com/gotd/contrib/storage"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/tg"
	"go.etcd.io/bbolt"
)

const (
	peersBucketName                   = "peers"
	dialogsSnapshotBucket             = "dialogs_snapshot"
	dialogsSnapshotKey                = "v1"
	dbOpenTimeout                     = time.Second
	dbFileMode            os.FileMode = 0o600
)

var (
	peersBucketBytes        = []byte(peersBucketName)
	dialogsSnapshotBuckets  = []byte(dialogsSnapshotBucket)
	dialogsSnapshotKeyBytes = []byte(dialogsSnapshotKey)
)

// DialogKind описывает тип сущности в снимке диалогов.
type DialogKind string

const (
	DialogKindUser    DialogKind = "user"
	DialogKindChat    DialogKind = "chat"
	DialogKindChannel DialogKind = "channel"
	DialogKindFolder  DialogKind = "folder"
)

// DialogRef фиксирует минимальную информацию о диалоге для офлайн-CLI.
type DialogRef struct {
	Kind DialogKind `json:"kind"`
	ID   int64      `json:"id"`
}

// Service инкапсулирует менеджер пиров и bbolt-хранилище.
type Service struct {
	db    *bbolt.DB
	store contribstorage.PeerStorage
	Mgr   *peers.Manager

	mu      sync.RWMutex
	dialogs []DialogRef
}

// New создаёт сервис пиров поверх bbolt и gotd peers.Manager.
// Сразу после открытия файла загружает сохранённый снимок диалогов (если есть),
// но не выполняет сетевые запросы.
func New(api *tg.Client, dbPath string) (*Service, error) {
	if api == nil {
		return nil, errors.New("peersmgr: api client is nil")
	}
	path := strings.TrimSpace(dbPath)
	if path == "" {
		return nil, errors.New("peersmgr: db path is empty")
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, os.ModePerm); err != nil {
			return nil, fmt.Errorf("peersmgr: ensure dir %q: %w", dir, err)
		}
	}

	db, err := bbolt.Open(path, dbFileMode, &bbolt.Options{Timeout: dbOpenTimeout})
	if err != nil {
		return nil, fmt.Errorf("peersmgr: open db: %w", err)
	}

	service := &Service{
		db:      db,
		store:   bboltdb.NewPeerStorage(db, peersBucketBytes),
		Mgr:     (peers.Options{}).Build(api),
		dialogs: make([]DialogRef, 0),
	}

	if loadErr := service.loadDialogsSnapshot(); loadErr != nil {
		_ = db.Close()
		return nil, loadErr
	}

	return service, nil
}

// Close закрывает файл базы данных.
func (s *Service) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Store возвращает персистентное хранилище пиров (для UpdateHook).
func (s *Service) Store() contribstorage.PeerStorage {
	return s.store
}

// Dialogs возвращает копию текущего офлайн-снимка диалогов.
func (s *Service) Dialogs() []DialogRef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.dialogs) == 0 {
		return nil
	}
	result := make([]DialogRef, len(s.dialogs))
	copy(result, s.dialogs)
	return result
}

// LoadFromStorage прогружает сохранённые peers из bbolt в оперативный peers.Manager.
func (s *Service) LoadFromStorage(ctx context.Context) error {
	iter, exists, err := s.iterateStoredPeers(ctx)
	if err != nil {
		if isJSONUnmarshalError(err) {
			_ = s.resetPeersBucket()
			return nil
		}
		return fmt.Errorf("peersmgr: iterate stored peers: %w", err)
	}
	if !exists {
		return nil
	}
	defer func() {
		_ = iter.Close()
	}()

	users := make([]tg.UserClass, 0)
	chats := make([]tg.ChatClass, 0)

	for iter.Next(ctx) {
		value := iter.Value()
		switch value.Key.Kind {
		case dialogs.User:
			user := value.User
			if user == nil {
				user = &tg.User{
					ID:         value.Key.ID,
					AccessHash: value.Key.AccessHash,
				}
			}
			users = append(users, user)
		case dialogs.Chat:
			chat := value.Chat
			if chat == nil {
				chat = &tg.Chat{ID: value.Key.ID}
			}
			chats = append(chats, chat)
		case dialogs.Channel:
			channel := value.Channel
			if channel == nil {
				channel = &tg.Channel{
					ID:         value.Key.ID,
					AccessHash: value.Key.AccessHash,
				}
			}
			chats = append(chats, channel)
		}
	}

	if err = iter.Err(); err != nil {
		return fmt.Errorf("peersmgr: iterate stored peers: %w", err)
	}
	if len(users) == 0 && len(chats) == 0 {
		return nil
	}
	return s.Mgr.Apply(ctx, users, chats)
}

// LookupPeer возвращает сохранённую сущность из персистентного хранилища.
// Для диалогов без сущности (например, папки) возвращает ok=false.
func (s *Service) LookupPeer(ctx context.Context, kind DialogKind, id int64) (contribstorage.Peer, bool, error) {
	var key contribstorage.PeerKey
	switch kind {
	case DialogKindUser:
		key = contribstorage.PeerKey{Kind: dialogs.User, ID: id}
	case DialogKindChat:
		key = contribstorage.PeerKey{Kind: dialogs.Chat, ID: id}
	case DialogKindChannel:
		key = contribstorage.PeerKey{Kind: dialogs.Channel, ID: id}
	default:
		return contribstorage.Peer{}, false, nil
	}

	value, err := s.store.Find(ctx, key)
	if errors.Is(err, contribstorage.ErrPeerNotFound) {
		return contribstorage.Peer{}, false, nil
	}
	if err != nil {
		return contribstorage.Peer{}, false, fmt.Errorf("lookup peer: %w", err)
	}
	return value, true, nil
}

// InputPeerFromMessage возвращает tg.InputPeerClass для сообщения.
func (s *Service) InputPeerFromMessage(ctx context.Context, msg *tg.Message) (tg.InputPeerClass, error) {
	if msg == nil {
		return nil, errors.New("peersmgr: message is nil")
	}

	switch peer := msg.PeerID.(type) {
	case *tg.PeerUser:
		user, err := s.Mgr.ResolveUserID(ctx, peer.UserID)
		if err != nil {
			return nil, fmt.Errorf("resolve user %d: %w", peer.UserID, err)
		}
		return user.InputPeer(), nil
	case *tg.PeerChat:
		chat, err := s.Mgr.ResolveChatID(ctx, peer.ChatID)
		if err != nil {
			return nil, fmt.Errorf("resolve chat %d: %w", peer.ChatID, err)
		}
		return chat.InputPeer(), nil
	case *tg.PeerChannel:
		channel, err := s.Mgr.ResolveChannelID(ctx, peer.ChannelID)
		if err != nil {
			return nil, fmt.Errorf("resolve channel %d: %w", peer.ChannelID, err)
		}
		return channel.InputPeer(), nil
	default:
		return nil, fmt.Errorf("peersmgr: unsupported peer type %T", peer)
	}
}

// InputPeerByKind подбирает tg.InputPeerClass по строковому типу и идентификатору.
func (s *Service) InputPeerByKind(ctx context.Context, kind string, id int64) (tg.InputPeerClass, error) {
	switch kind {
	case "user":
		user, err := s.Mgr.ResolveUserID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("resolve user %d: %w", id, err)
		}
		return user.InputPeer(), nil
	case "chat":
		chat, err := s.Mgr.ResolveChatID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("resolve chat %d: %w", id, err)
		}
		return chat.InputPeer(), nil
	case "channel":
		channel, err := s.Mgr.ResolveChannelID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("resolve channel %d: %w", id, err)
		}
		return channel.InputPeer(), nil
	default:
		return nil, fmt.Errorf("peersmgr: unsupported peer kind %q", kind)
	}
}

// ResolvePeer возвращает peers.Peer для указанного диалога; ok=false, если нет данных.
func (s *Service) ResolvePeer(ctx context.Context, kind DialogKind, id int64) (peers.Peer, bool, error) {
	switch kind {
	case DialogKindUser:
		user, err := s.Mgr.ResolveUserID(ctx, id)
		if err != nil {
			var nf *peers.PeerNotFoundError
			if errors.As(err, &nf) {
				return nil, false, nil
			}
			return nil, false, err
		}
		return user, true, nil
	case DialogKindChat:
		chat, err := s.Mgr.ResolveChatID(ctx, id)
		if err != nil {
			return nil, false, err
		}
		return chat, true, nil
	case DialogKindChannel:
		channel, err := s.Mgr.ResolveChannelID(ctx, id)
		if err != nil {
			var nf *peers.PeerNotFoundError
			if errors.As(err, &nf) {
				return nil, false, nil
			}
			return nil, false, err
		}
		return channel, true, nil
	case DialogKindFolder:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("peersmgr: unsupported dialog kind %q", kind)
	}
}

func (s *Service) applyEntities(ctx context.Context, entities tg.Entities) error {
	if len(entities.Users) == 0 && len(entities.Chats) == 0 {
		return nil
	}

	users := make([]tg.UserClass, 0, len(entities.Users))
	for _, u := range entities.Users {
		if u != nil {
			users = append(users, u)
		}
	}

	chats := make([]tg.ChatClass, 0, len(entities.Chats))
	for _, ch := range entities.Chats {
		if ch != nil {
			chats = append(chats, ch)
		}
	}

	if len(users) == 0 && len(chats) == 0 {
		return nil
	}
	return s.Mgr.Apply(ctx, users, chats)
}

// RefreshDialogs пере Fetch диалогов, обновляет peers.Manager, персистентное хранилище и снимок.
func (s *Service) RefreshDialogs(ctx context.Context, api *tg.Client) error {
	client := s.selectAPI(api)
	if client == nil {
		return errors.New("peersmgr: telegram client is nil")
	}

	dialogs, err := fetchDialogs(ctx, client)
	if err != nil {
		return fmt.Errorf("peersmgr: fetch dialogs: %w", err)
	}

	if err = s.Mgr.Apply(ctx, dialogs.Users, dialogs.Chats); err != nil {
		return fmt.Errorf("peersmgr: apply entities: %w", err)
	}
	if err = s.saveDialogsSnapshot(dialogs.Dialogs); err != nil {
		return fmt.Errorf("peersmgr: persist dialogs snapshot: %w", err)
	}
	return nil
}

// selectAPI выбирает приоритетный tg.Client: переданный явно или из менеджера.
func (s *Service) selectAPI(explicit *tg.Client) *tg.Client {
	if explicit != nil {
		return explicit
	}
	if s.Mgr != nil {
		return s.Mgr.API()
	}
	return nil
}

func (s *Service) iterateStoredPeers(ctx context.Context) (contribstorage.PeerIterator, bool, error) {
	exists := false
	if err := s.db.View(func(tx *bbolt.Tx) error {
		exists = tx.Bucket(peersBucketBytes) != nil
		return nil
	}); err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, false, nil
	}
	iter, err := s.store.Iterate(ctx)
	if err != nil {
		return nil, false, err
	}
	return iter, true, nil
}

func isJSONUnmarshalError(err error) bool {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return true
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return true
	}
	return strings.Contains(err.Error(), "json:")
}

func (s *Service) resetPeersBucket() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.DeleteBucket(peersBucketBytes); err != nil && !errors.Is(err, bbolt.ErrBucketNotFound) {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(peersBucketBytes)
		return err
	})
}

func (s *Service) loadDialogsSnapshot() error {
	var data []byte
	if err := s.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(dialogsSnapshotBuckets)
		if bucket == nil {
			return nil
		}
		value := bucket.Get(dialogsSnapshotKeyBytes)
		if len(value) == 0 {
			return nil
		}
		data = append(data, value...)
		return nil
	}); err != nil {
		return fmt.Errorf("peersmgr: load snapshot: %w", err)
	}

	if len(data) == 0 {
		s.setDialogs(nil)
		return nil
	}

	var refs []DialogRef
	if err := json.Unmarshal(data, &refs); err != nil {
		return fmt.Errorf("peersmgr: decode snapshot: %w", err)
	}
	s.setDialogs(refs)
	return nil
}

func (s *Service) saveDialogsSnapshot(source []tg.DialogClass) error {
	refs := make([]DialogRef, 0, len(source))
	for _, dialog := range source {
		switch dlg := dialog.(type) {
		case *tg.Dialog:
			switch peer := dlg.Peer.(type) {
			case *tg.PeerUser:
				refs = append(refs, DialogRef{Kind: DialogKindUser, ID: peer.UserID})
			case *tg.PeerChat:
				refs = append(refs, DialogRef{Kind: DialogKindChat, ID: peer.ChatID})
			case *tg.PeerChannel:
				refs = append(refs, DialogRef{Kind: DialogKindChannel, ID: peer.ChannelID})
			}
		case *tg.DialogFolder:
			if !dlg.Folder.Zero() {
				refs = append(refs, DialogRef{Kind: DialogKindFolder, ID: int64(dlg.Folder.ID)})
			}
		}
	}

	payload, err := json.Marshal(refs)
	if err != nil {
		return fmt.Errorf("peersmgr: marshal snapshot: %w", err)
	}

	err = s.db.Update(func(tx *bbolt.Tx) error {
		bucket, bucketErr := tx.CreateBucketIfNotExists(dialogsSnapshotBuckets)
		if bucketErr != nil {
			return bucketErr
		}
		return bucket.Put(dialogsSnapshotKeyBytes, payload)
	})
	if err != nil {
		return fmt.Errorf("peersmgr: save snapshot: %w", err)
	}
	s.setDialogs(refs)
	return nil
}

func (s *Service) setDialogs(refs []DialogRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(refs) == 0 {
		s.dialogs = nil
		return
	}
	s.dialogs = make([]DialogRef, len(refs))
	copy(s.dialogs, refs)
}
