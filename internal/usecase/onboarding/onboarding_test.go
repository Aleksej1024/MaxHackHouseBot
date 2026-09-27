package onboarding

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
)

// --- Тестовые дублёры портов ---

// stubDirectory отдаёт заданный список чатов (stub) и запоминает ники.
type stubDirectory struct {
	chats     map[int64][]housechat.Chat
	nicknames map[int64]string
	listErr   error
	nickErr   error
}

func newStubDirectory() *stubDirectory {
	return &stubDirectory{chats: map[int64][]housechat.Chat{}, nicknames: map[int64]string{}}
}

func (d *stubDirectory) UpdateNickname(_ context.Context, userID int64, nickname string) error {
	if d.nickErr != nil {
		return d.nickErr
	}
	d.nicknames[userID] = nickname
	return nil
}

func (d *stubDirectory) ListActiveChats(_ context.Context, userID int64) ([]housechat.Chat, error) {
	return d.chats[userID], d.listErr
}

// fakeStore — in-memory FSMStore; err имитирует недоступный Redis.
type fakeStore struct {
	data map[string]string
	ttl  map[string]time.Duration
	err  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{data: map[string]string{}, ttl: map[string]time.Duration{}}
}

func (s *fakeStore) Get(_ context.Context, key string) (string, bool, error) {
	if s.err != nil {
		return "", false, s.err
	}
	v, ok := s.data[key]
	return v, ok, nil
}

func (s *fakeStore) Set(_ context.Context, key, value string, ttl time.Duration) error {
	if s.err != nil {
		return s.err
	}
	s.data[key] = value
	s.ttl[key] = ttl
	return nil
}

func (s *fakeStore) Delete(_ context.Context, key string) error {
	if s.err != nil {
		return s.err
	}
	delete(s.data, key)
	delete(s.ttl, key)
	return nil
}

const (
	userID     = int64(7)
	sessionTTL = time.Hour
)

var (
	chatA = housechat.Chat{ID: 100, Title: "Дом А", Status: housechat.StatusActive}
	chatB = housechat.Chat{ID: 200, Title: "Дом Б", Status: housechat.StatusActive}
)

func newService(dir *stubDirectory, store *fakeStore) *Service {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(dir, store, sessionTTL, log)
}

// --- Start ---

func TestStart_UnknownUser(t *testing.T) {
	svc := newService(newStubDirectory(), newFakeStore())

	res, err := svc.Start(context.Background(), membership.User{ID: userID, Nickname: "n"})
	require.NoError(t, err)
	assert.Equal(t, OutcomeNotRegistered, res.Outcome)
}

func TestStart_SingleChat(t *testing.T) {
	dir := newStubDirectory()
	dir.chats[userID] = []housechat.Chat{chatA}
	store := newFakeStore()
	svc := newService(dir, store)

	res, err := svc.Start(context.Background(), membership.User{ID: userID, Nickname: "ник"})
	require.NoError(t, err)
	assert.Equal(t, OutcomeReady, res.Outcome)
	assert.Equal(t, chatA, res.Chat)
	assert.True(t, res.Greet)
	assert.False(t, res.CanSwitch())

	assert.Equal(t, "ник", dir.nicknames[userID], "ник обновляется из апдейта")
	assert.Equal(t, "100", store.data["session:7:chat_id"])
	assert.Equal(t, sessionTTL, store.ttl["session:7:chat_id"])
}

func TestStart_GreetingOncePerSession(t *testing.T) {
	dir := newStubDirectory()
	dir.chats[userID] = []housechat.Chat{chatA}
	store := newFakeStore()
	svc := newService(dir, store)
	ctx := context.Background()

	res, err := svc.Start(ctx, membership.User{ID: userID})
	require.NoError(t, err)
	assert.True(t, res.Greet)

	res, err = svc.Start(ctx, membership.User{ID: userID})
	require.NoError(t, err)
	assert.False(t, res.Greet, "повторно приветствие не показываем")

	// Сессия истекла (TTL в Redis) — приветствие снова.
	store.data = map[string]string{}
	res, err = svc.Start(ctx, membership.User{ID: userID})
	require.NoError(t, err)
	assert.True(t, res.Greet)
}

func TestStart_MultipleChatsChooseThenRemember(t *testing.T) {
	dir := newStubDirectory()
	dir.chats[userID] = []housechat.Chat{chatA, chatB}
	svc := newService(dir, newFakeStore())
	ctx := context.Background()

	res, err := svc.Start(ctx, membership.User{ID: userID})
	require.NoError(t, err)
	assert.Equal(t, OutcomeChooseChat, res.Outcome)
	assert.Equal(t, []housechat.Chat{chatA, chatB}, res.Chats)

	res, err = svc.SelectChat(ctx, userID, chatB.ID)
	require.NoError(t, err)
	assert.Equal(t, OutcomeReady, res.Outcome)
	assert.Equal(t, chatB, res.Chat)
	assert.True(t, res.Greet)
	assert.True(t, res.CanSwitch())

	res, err = svc.Start(ctx, membership.User{ID: userID})
	require.NoError(t, err)
	assert.Equal(t, OutcomeReady, res.Outcome)
	assert.Equal(t, chatB, res.Chat, "выбор сохраняется в сессии")
	assert.False(t, res.Greet)
}

func TestStart_SessionChatNoLongerAvailable(t *testing.T) {
	dir := newStubDirectory()
	dir.chats[userID] = []housechat.Chat{chatA, chatB}
	store := newFakeStore()
	store.data["session:7:chat_id"] = "999"
	svc := newService(dir, store)

	res, err := svc.Start(context.Background(), membership.User{ID: userID})
	require.NoError(t, err)
	assert.Equal(t, OutcomeChooseChat, res.Outcome)
}

func TestStart_RedisUnavailable(t *testing.T) {
	dir := newStubDirectory()
	dir.chats[userID] = []housechat.Chat{chatA}
	store := newFakeStore()
	store.err = errors.New("redis down")
	svc := newService(dir, store)

	res, err := svc.Start(context.Background(), membership.User{ID: userID})
	require.NoError(t, err, "недоступный Redis не ломает сценарий")
	assert.Equal(t, OutcomeReady, res.Outcome)
	assert.Equal(t, chatA, res.Chat)
}

func TestStart_NicknameErrorIgnored(t *testing.T) {
	dir := newStubDirectory()
	dir.chats[userID] = []housechat.Chat{chatA}
	dir.nickErr = errors.New("pg down")
	svc := newService(dir, newFakeStore())

	res, err := svc.Start(context.Background(), membership.User{ID: userID})
	require.NoError(t, err)
	assert.Equal(t, OutcomeReady, res.Outcome)
}

func TestStart_DirectoryError(t *testing.T) {
	dir := newStubDirectory()
	dir.listErr = errors.New("pg down")
	svc := newService(dir, newFakeStore())

	_, err := svc.Start(context.Background(), membership.User{ID: userID})
	require.Error(t, err)
}

// --- SelectChat ---

func TestSelectChat_ForeignChatRejected(t *testing.T) {
	dir := newStubDirectory()
	dir.chats[userID] = []housechat.Chat{chatA, chatB}
	store := newFakeStore()
	svc := newService(dir, store)

	res, err := svc.SelectChat(context.Background(), userID, 999)
	require.NoError(t, err)
	assert.Equal(t, OutcomeChooseChat, res.Outcome)
	_, ok := store.data["session:7:chat_id"]
	assert.False(t, ok, "чужой чат не сохраняется в сессии")
}

func TestSelectChat_NotRegistered(t *testing.T) {
	svc := newService(newStubDirectory(), newFakeStore())

	res, err := svc.SelectChat(context.Background(), userID, chatA.ID)
	require.NoError(t, err)
	assert.Equal(t, OutcomeNotRegistered, res.Outcome)
}

// --- SwitchChat ---

func TestSwitchChat(t *testing.T) {
	tests := []struct {
		name  string
		chats []housechat.Chat
		want  Outcome
	}{
		{name: "нет чатов", chats: nil, want: OutcomeNotRegistered},
		{name: "один чат", chats: []housechat.Chat{chatA}, want: OutcomeReady},
		{name: "несколько чатов", chats: []housechat.Chat{chatA, chatB}, want: OutcomeChooseChat},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := newStubDirectory()
			dir.chats[userID] = tt.chats
			svc := newService(dir, newFakeStore())

			res, err := svc.SwitchChat(context.Background(), userID)
			require.NoError(t, err)
			assert.Equal(t, tt.want, res.Outcome)
		})
	}
}

// --- ActiveChat ---

func TestActiveChat(t *testing.T) {
	ctx := context.Background()

	t.Run("единственный чат выбирается автоматически", func(t *testing.T) {
		dir := newStubDirectory()
		dir.chats[userID] = []housechat.Chat{chatA}
		svc := newService(dir, newFakeStore())

		chat, ok, err := svc.ActiveChat(ctx, userID)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, chatA, chat)
	})

	t.Run("несколько чатов без выбора", func(t *testing.T) {
		dir := newStubDirectory()
		dir.chats[userID] = []housechat.Chat{chatA, chatB}
		svc := newService(dir, newFakeStore())

		_, ok, err := svc.ActiveChat(ctx, userID)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("выбранный чат из сессии", func(t *testing.T) {
		dir := newStubDirectory()
		dir.chats[userID] = []housechat.Chat{chatA, chatB}
		store := newFakeStore()
		store.data["session:7:chat_id"] = "200"
		svc := newService(dir, store)

		chat, ok, err := svc.ActiveChat(ctx, userID)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, chatB, chat)
	})

	t.Run("пользователь вышел из чата", func(t *testing.T) {
		dir := newStubDirectory()
		store := newFakeStore()
		store.data["session:7:chat_id"] = "100"
		svc := newService(dir, store)

		_, ok, err := svc.ActiveChat(ctx, userID)
		require.NoError(t, err)
		assert.False(t, ok)
	})
}

// --- LeftChat ---

func TestLeftChat(t *testing.T) {
	ctx := context.Background()
	chatC := housechat.Chat{ID: 300, Title: "Дом В", Status: housechat.StatusActive}

	t.Run("единственный дом — сессия сброшена целиком", func(t *testing.T) {
		dir := newStubDirectory()
		dir.chats[userID] = []housechat.Chat{chatA}
		store := newFakeStore()
		svc := newService(dir, store)
		_, err := svc.Start(ctx, membership.User{ID: userID})
		require.NoError(t, err)

		dir.chats[userID] = nil // вышел, статус left
		switched, err := svc.LeftChat(ctx, userID, chatA.ID)
		require.NoError(t, err)
		assert.True(t, switched)
		assert.Empty(t, store.data, "дом и приветствие удалены")
	})

	t.Run("несколько домов — активным становится оставшийся", func(t *testing.T) {
		dir := newStubDirectory()
		dir.chats[userID] = []housechat.Chat{chatA, chatB, chatC}
		store := newFakeStore()
		svc := newService(dir, store)
		_, err := svc.SelectChat(ctx, userID, chatB.ID)
		require.NoError(t, err)

		dir.chats[userID] = []housechat.Chat{chatA, chatC}
		switched, err := svc.LeftChat(ctx, userID, chatB.ID)
		require.NoError(t, err)
		assert.True(t, switched)
		assert.Equal(t, "100", store.data["session:7:chat_id"], "первый из оставшихся")
		assert.Equal(t, sessionTTL, store.ttl["session:7:chat_id"])
		assert.Equal(t, "1", store.data["session:7:greeted"], "приветствие повторно не нужно")

		// Следующее обращение — сразу в оставшийся дом, без выбора.
		res, err := svc.Start(ctx, membership.User{ID: userID})
		require.NoError(t, err)
		assert.Equal(t, OutcomeReady, res.Outcome)
		assert.Equal(t, chatA, res.Chat)
	})

	t.Run("вышел из невыбранного дома — сессия не меняется", func(t *testing.T) {
		dir := newStubDirectory()
		dir.chats[userID] = []housechat.Chat{chatA, chatB}
		store := newFakeStore()
		svc := newService(dir, store)
		_, err := svc.SelectChat(ctx, userID, chatA.ID)
		require.NoError(t, err)

		dir.chats[userID] = []housechat.Chat{chatA}
		switched, err := svc.LeftChat(ctx, userID, chatB.ID)
		require.NoError(t, err)
		assert.False(t, switched, "диалог в выбранном доме не прерываем")
		assert.Equal(t, "100", store.data["session:7:chat_id"])
	})

	t.Run("сессии нет", func(t *testing.T) {
		svc := newService(newStubDirectory(), newFakeStore())
		switched, err := svc.LeftChat(ctx, userID, chatA.ID)
		require.NoError(t, err)
		assert.False(t, switched)
	})

	t.Run("Redis недоступен", func(t *testing.T) {
		store := newFakeStore()
		store.err = errors.New("redis down")
		_, err := newService(newStubDirectory(), store).LeftChat(ctx, userID, chatA.ID)
		require.Error(t, err)
	})
}
