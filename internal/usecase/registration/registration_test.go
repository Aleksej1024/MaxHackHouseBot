package registration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/usecase"
)

// --- Тестовые дублёры портов ---

type fakeChatRepo struct {
	chats map[int64]housechat.Chat
}

func newFakeChatRepo() *fakeChatRepo {
	return &fakeChatRepo{chats: map[int64]housechat.Chat{}}
}

func (r *fakeChatRepo) Upsert(_ context.Context, chat housechat.Chat) error {
	if existing, ok := r.chats[chat.ID]; ok {
		existing.Title = chat.Title // при конфликте только title
		r.chats[chat.ID] = existing
		return nil
	}
	r.chats[chat.ID] = chat
	return nil
}

func (r *fakeChatRepo) Get(_ context.Context, chatID int64) (housechat.Chat, error) {
	chat, ok := r.chats[chatID]
	if !ok {
		return housechat.Chat{}, usecase.ErrNotFound
	}
	return chat, nil
}

func (r *fakeChatRepo) SetStatus(_ context.Context, chatID int64, status housechat.Status) error {
	if chat, ok := r.chats[chatID]; ok {
		chat.Status = status
		r.chats[chatID] = chat
	}
	return nil
}

type membershipKey struct{ chatID, userID int64 }

type fakeMembershipRepo struct {
	users       map[int64]membership.User
	memberships map[membershipKey]membership.Membership
}

func newFakeMembershipRepo() *fakeMembershipRepo {
	return &fakeMembershipRepo{
		users:       map[int64]membership.User{},
		memberships: map[membershipKey]membership.Membership{},
	}
}

func (r *fakeMembershipRepo) UpsertUser(_ context.Context, u membership.User) error {
	r.users[u.ID] = u // при конфликте обновляет nickname
	return nil
}

func (r *fakeMembershipRepo) UpsertMembership(_ context.Context, m membership.Membership) error {
	key := membershipKey{m.ChatID, m.UserID}
	if existing, ok := r.memberships[key]; ok {
		// При конфликте только status = active; rating и role сохраняются.
		existing.Status = membership.StatusActive
		r.memberships[key] = existing
		return nil
	}
	r.memberships[key] = m
	return nil
}

func (r *fakeMembershipRepo) SetStatus(_ context.Context, chatID, userID int64, status membership.Status) error {
	key := membershipKey{chatID, userID}
	if m, ok := r.memberships[key]; ok {
		m.Status = status
		r.memberships[key] = m
	}
	return nil
}

type fakeMemberLister struct {
	members  []membership.Member
	err      error
	title    string
	titleErr error
}

func (l *fakeMemberLister) ListMembers(context.Context, int64) ([]membership.Member, error) {
	return l.members, l.err
}

func (l *fakeMemberLister) ChatTitle(context.Context, int64) (string, error) {
	return l.title, l.titleErr
}

// mockSessions запоминает вызовы LeftChat (user_id, chat_id).
type mockSessions struct {
	left [][2]int64
	err  error
}

func (m *mockSessions) LeftChat(_ context.Context, userID, chatID int64) error {
	m.left = append(m.left, [2]int64{userID, chatID})
	return m.err
}

type fakeTxManager struct{}

func (fakeTxManager) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// --- Сборка ---

type fixture struct {
	chats    *fakeChatRepo
	membs    *fakeMembershipRepo
	lister   *fakeMemberLister
	sessions *mockSessions
}

func newService(t *testing.T, autoActivate bool, f *fixture) *Service {
	t.Helper()
	f.chats = newFakeChatRepo()
	f.membs = newFakeMembershipRepo()
	f.lister = &fakeMemberLister{}
	f.sessions = &mockSessions{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(f.chats, f.membs, f.lister, f.sessions, fakeTxManager{}, autoActivate, log)
}

// --- BotAdded ---

func TestBotAdded_NewChatAutoActivate(t *testing.T) {
	var f fixture
	svc := newService(t, true, &f)

	status, err := svc.BotAdded(context.Background(), 100, "Дом 1")
	require.NoError(t, err)
	assert.Equal(t, housechat.StatusActive, status)
	assert.Equal(t, housechat.StatusActive, f.chats.chats[100].Status)
}

func TestBotAdded_NewChatManualActivation(t *testing.T) {
	var f fixture
	svc := newService(t, false, &f)

	status, err := svc.BotAdded(context.Background(), 100, "Дом 1")
	require.NoError(t, err)
	assert.Equal(t, housechat.StatusPending, status)
	assert.Equal(t, housechat.StatusPending, f.chats.chats[100].Status)
}

func TestBotAdded_RepeatedKeepsStatusTitleAndRating(t *testing.T) {
	var f fixture
	svc := newService(t, true, &f)
	f.lister.members = []membership.Member{{UserID: 7, Nickname: "n7"}}

	_, err := svc.BotAdded(context.Background(), 100, "Дом 1")
	require.NoError(t, err)
	// Имитируем накопленный рейтинг и админскую роль.
	m := f.membs.memberships[membershipKey{100, 7}]
	m.Rating = 5
	m.Role = membership.RoleAdmin
	f.membs.memberships[membershipKey{100, 7}] = m

	status, err := svc.BotAdded(context.Background(), 100, "Дом 1 новый")
	require.NoError(t, err)
	assert.Equal(t, housechat.StatusActive, status)
	assert.Equal(t, "Дом 1 новый", f.chats.chats[100].Title)

	got := f.membs.memberships[membershipKey{100, 7}]
	assert.Equal(t, 5, got.Rating, "рейтинг не должен сбрасываться")
	assert.Equal(t, membership.RoleAdmin, got.Role, "роль не должна сбрасываться")
}

func TestBotAdded_ReactivationAfterBotRemoved(t *testing.T) {
	var f fixture
	svc := newService(t, false, &f)
	f.chats.chats[100] = housechat.Chat{ID: 100, Title: "Дом", Status: housechat.StatusBotRemoved}

	status, err := svc.BotAdded(context.Background(), 100, "Дом")
	require.NoError(t, err)
	assert.Equal(t, housechat.StatusPending, status)
	assert.Equal(t, housechat.StatusPending, f.chats.chats[100].Status)
}

func TestBotAdded_MembersSavedBotsSkipped(t *testing.T) {
	var f fixture
	svc := newService(t, true, &f)
	f.lister.members = []membership.Member{
		{UserID: 1, Nickname: "owner", IsAdmin: true},
		{UserID: 2, Nickname: "plain"},
		{UserID: 3, Nickname: "botfriend", IsBot: true},
	}

	_, err := svc.BotAdded(context.Background(), 100, "Дом")
	require.NoError(t, err)

	admin := f.membs.memberships[membershipKey{100, 1}]
	assert.Equal(t, membership.RoleAdmin, admin.Role)
	assert.Equal(t, membership.StatusActive, admin.Status)
	assert.Equal(t, membership.RoleMember, f.membs.memberships[membershipKey{100, 2}].Role)
	_, ok := f.membs.memberships[membershipKey{100, 3}]
	assert.False(t, ok, "боты не сохраняются как участники")
	_, ok = f.membs.users[3]
	assert.False(t, ok)
}

func TestBotAdded_ListMembersError(t *testing.T) {
	var f fixture
	svc := newService(t, true, &f)
	f.lister.err = errors.New("max недоступен")

	_, err := svc.BotAdded(context.Background(), 100, "Дом")
	require.Error(t, err)
}

func TestBotAdded_Idempotent(t *testing.T) {
	var f fixture
	svc := newService(t, true, &f)
	f.lister.members = []membership.Member{{UserID: 1, Nickname: "a"}}

	for i := 0; i < 3; i++ {
		status, err := svc.BotAdded(context.Background(), 100, "Дом")
		require.NoError(t, err)
		assert.Equal(t, housechat.StatusActive, status)
	}
	assert.Len(t, f.membs.memberships, 1)
}

// --- BotRemoved ---

func TestBotRemoved(t *testing.T) {
	var f fixture
	svc := newService(t, true, &f)
	f.chats.chats[100] = housechat.Chat{ID: 100, Status: housechat.StatusActive}

	require.NoError(t, svc.BotRemoved(context.Background(), 100))
	assert.Equal(t, housechat.StatusBotRemoved, f.chats.chats[100].Status)

	// Повтор и неизвестный чат — не ошибка.
	require.NoError(t, svc.BotRemoved(context.Background(), 100))
	require.NoError(t, svc.BotRemoved(context.Background(), 999))
}

// --- UserAdded / UserRemoved ---

func TestUserAdded_CreatesUserAndMembership(t *testing.T) {
	var f fixture
	svc := newService(t, true, &f)
	f.chats.chats[100] = housechat.Chat{ID: 100, Status: housechat.StatusActive}

	err := svc.UserAdded(context.Background(), 100, membership.User{ID: 7, Nickname: "n7"})
	require.NoError(t, err)

	assert.Equal(t, "n7", f.membs.users[7].Nickname)
	m := f.membs.memberships[membershipKey{100, 7}]
	assert.Equal(t, membership.RoleMember, m.Role)
	assert.Equal(t, membership.StatusActive, m.Status)
}

func TestUserAdded_UnknownChatSkipped(t *testing.T) {
	var f fixture
	svc := newService(t, true, &f)

	err := svc.UserAdded(context.Background(), 100, membership.User{ID: 7})
	require.NoError(t, err)
	assert.Empty(t, f.membs.memberships)
	assert.Empty(t, f.membs.users)
}

func TestUserRemovedThenRejoinKeepsRating(t *testing.T) {
	var f fixture
	svc := newService(t, true, &f)
	f.chats.chats[100] = housechat.Chat{ID: 100, Status: housechat.StatusActive}
	ctx := context.Background()

	require.NoError(t, svc.UserAdded(ctx, 100, membership.User{ID: 7, Nickname: "n7"}))
	key := membershipKey{100, 7}
	m := f.membs.memberships[key]
	m.Rating = 4
	f.membs.memberships[key] = m

	require.NoError(t, svc.UserRemoved(ctx, 100, 7))
	assert.Equal(t, membership.StatusLeft, f.membs.memberships[key].Status)
	assert.Equal(t, [][2]int64{{7, 100}}, f.sessions.left, "сессия вышедшего обновлена")

	require.NoError(t, svc.UserAdded(ctx, 100, membership.User{ID: 7, Nickname: "n7"}))
	got := f.membs.memberships[key]
	assert.Equal(t, membership.StatusActive, got.Status)
	assert.Equal(t, 4, got.Rating, "рейтинг сохраняется при повторном входе")

	// Повторный выход — не ошибка.
	require.NoError(t, svc.UserRemoved(ctx, 100, 7))
	require.NoError(t, svc.UserRemoved(ctx, 100, 404))
}

func TestUserRemoved_SessionFailureIgnored(t *testing.T) {
	var f fixture
	svc := newService(t, true, &f)
	f.chats.chats[100] = housechat.Chat{ID: 100, Status: housechat.StatusActive}
	ctx := context.Background()
	require.NoError(t, svc.UserAdded(ctx, 100, membership.User{ID: 7}))
	f.sessions.err = errors.New("redis down")

	require.NoError(t, svc.UserRemoved(ctx, 100, 7), "сбой Redis не мешает выходу")
	assert.Equal(t, membership.StatusLeft, f.membs.memberships[membershipKey{100, 7}].Status)
}

// --- Название чата ---

func TestBotAdded_TitleFetchedFromMax(t *testing.T) {
	var f fixture
	svc := newService(t, true, &f)
	f.lister.title = "Ленина, 5"

	_, err := svc.BotAdded(context.Background(), 100, "")
	require.NoError(t, err)
	assert.Equal(t, "Ленина, 5", f.chats.chats[100].Title, "в bot_added названия нет — берём у MAX")
}

func TestBotAdded_TitleFetchErrorNotFatal(t *testing.T) {
	var f fixture
	svc := newService(t, true, &f)
	f.lister.titleErr = errors.New("max недоступен")

	status, err := svc.BotAdded(context.Background(), 100, "")
	require.NoError(t, err)
	assert.Equal(t, housechat.StatusActive, status)
}

func TestChatTitleChanged(t *testing.T) {
	var f fixture
	svc := newService(t, true, &f)
	f.chats.chats[100] = housechat.Chat{ID: 100, Title: "Старое", Status: housechat.StatusPending}

	require.NoError(t, svc.ChatTitleChanged(context.Background(), 100, "Новое"))
	assert.Equal(t, housechat.Chat{ID: 100, Title: "Новое", Status: housechat.StatusPending}, f.chats.chats[100])

	require.NoError(t, svc.ChatTitleChanged(context.Background(), 404, "Чужой"))
	_, ok := f.chats.chats[404]
	assert.False(t, ok, "неизвестный чат не создаётся")
}
