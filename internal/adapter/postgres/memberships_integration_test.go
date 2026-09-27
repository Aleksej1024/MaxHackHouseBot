//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/usecase"
)

// --- ChatRepo ---

func TestChatRepo_UpsertKeepsStatus(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	repo := NewChatRepo(testPool)

	require.NoError(t, repo.Upsert(ctx, housechat.Chat{ID: -100, Title: "Старое", Status: housechat.StatusPending}))
	require.NoError(t, repo.Upsert(ctx, housechat.Chat{ID: -100, Title: "Новое", Status: housechat.StatusActive}))

	chat, err := repo.Get(ctx, -100)
	require.NoError(t, err)
	assert.Equal(t, housechat.Chat{ID: -100, Title: "Новое", Status: housechat.StatusPending}, chat,
		"при конфликте меняется только title")
}

func TestChatRepo_GetNotFoundAndSetStatus(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	repo := NewChatRepo(testPool)

	_, err := repo.Get(ctx, 404)
	require.ErrorIs(t, err, usecase.ErrNotFound)

	seedChat(t, -100, housechat.StatusActive)
	require.NoError(t, repo.SetStatus(ctx, -100, housechat.StatusBotRemoved))
	chat, err := repo.Get(ctx, -100)
	require.NoError(t, err)
	assert.Equal(t, housechat.StatusBotRemoved, chat.Status)

	assert.NoError(t, repo.SetStatus(ctx, 404, housechat.StatusActive), "отсутствие чата — не ошибка")
}

// --- MembershipRepo: регистрация ---

func TestMembershipRepo_UpsertKeepsRatingRoleAndJoinedAt(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	repo := NewMembershipRepo(testPool)
	seedChat(t, -100, housechat.StatusActive)
	seedUser(t, 7, "n7")

	require.NoError(t, repo.UpsertMembership(ctx, membership.Membership{
		ChatID: -100, UserID: 7, Role: membership.RoleAdmin, Status: membership.StatusActive,
	}))
	_, err := testPool.Exec(ctx, `UPDATE memberships SET rating = 5, joined_at = '2020-01-01' WHERE user_id = 7`)
	require.NoError(t, err)
	require.NoError(t, repo.SetStatus(ctx, -100, 7, membership.StatusLeft))

	// Повторное добавление бота / повторный вход.
	require.NoError(t, repo.UpsertMembership(ctx, membership.Membership{
		ChatID: -100, UserID: 7, Role: membership.RoleMember, Status: membership.StatusActive,
	}))

	var role, status string
	var rating, year int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT role, rating, status, extract(year FROM joined_at)::int FROM memberships WHERE user_id = 7`,
	).Scan(&role, &rating, &status, &year))
	assert.Equal(t, "admin", role, "роль не сбрасывается")
	assert.Equal(t, 5, rating, "рейтинг не обнуляется")
	assert.Equal(t, "active", status)
	assert.Equal(t, 2020, year, "joined_at сохраняется")
}

func TestMembershipRepo_UpsertUserUpdatesNickname(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	repo := NewMembershipRepo(testPool)

	require.NoError(t, repo.UpsertUser(ctx, membership.User{ID: 7, Nickname: "старый"}))
	require.NoError(t, repo.UpsertUser(ctx, membership.User{ID: 7, Nickname: "новый"}))

	var nick string
	require.NoError(t, testPool.QueryRow(ctx, `SELECT nickname FROM users WHERE id = 7`).Scan(&nick))
	assert.Equal(t, "новый", nick)
}

// --- MembershipRepo: личка ---

func TestMembershipRepo_UpdateNickname(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	repo := NewMembershipRepo(testPool)
	seedUser(t, 7, "старый")
	require.NoError(t, repo.MarkDMUnavailable(ctx, 7))

	require.NoError(t, repo.UpdateNickname(ctx, 7, "новый"))
	require.NoError(t, repo.UpdateNickname(ctx, 999, "незнакомец"))

	var nick string
	var unavailable bool
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT nickname, dm_unavailable FROM users WHERE id = 7`).Scan(&nick, &unavailable))
	assert.Equal(t, "новый", nick)
	assert.False(t, unavailable, "пользователь написал боту — личка снова доступна")

	var n int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = 999`).Scan(&n))
	assert.Zero(t, n, "незнакомые пользователи не сохраняются")

	// Тот же ник, но флаг стоит — тоже сбрасывается.
	require.NoError(t, repo.MarkDMUnavailable(ctx, 7))
	require.NoError(t, repo.UpdateNickname(ctx, 7, "новый"))
	require.NoError(t, testPool.QueryRow(ctx, `SELECT dm_unavailable FROM users WHERE id = 7`).Scan(&unavailable))
	assert.False(t, unavailable)
}

func TestMembershipRepo_ListActiveChats(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	repo := NewMembershipRepo(testPool)
	chats := NewChatRepo(testPool)

	require.NoError(t, chats.Upsert(ctx, housechat.Chat{ID: 1, Title: "Б", Status: housechat.StatusActive}))
	require.NoError(t, chats.Upsert(ctx, housechat.Chat{ID: 2, Title: "А", Status: housechat.StatusActive}))
	require.NoError(t, chats.Upsert(ctx, housechat.Chat{ID: 3, Title: "В ожидании", Status: housechat.StatusPending}))
	require.NoError(t, chats.Upsert(ctx, housechat.Chat{ID: 4, Title: "Удалён", Status: housechat.StatusBotRemoved}))
	require.NoError(t, chats.Upsert(ctx, housechat.Chat{ID: 5, Title: "Вышел", Status: housechat.StatusActive}))
	for _, id := range []int64{1, 2, 3, 4, 5} {
		seedMember(t, id, 7)
	}
	require.NoError(t, repo.SetStatus(ctx, 5, 7, membership.StatusLeft))

	list, err := repo.ListActiveChats(ctx, 7)
	require.NoError(t, err)
	assert.Equal(t, []housechat.Chat{
		{ID: 2, Title: "А", Status: housechat.StatusActive},
		{ID: 1, Title: "Б", Status: housechat.StatusActive},
	}, list, "только active-чаты с активным участием, по title")

	list, err = repo.ListActiveChats(ctx, 999)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestMembershipRepo_GetActive(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	repo := NewMembershipRepo(testPool)
	seedChat(t, 1, housechat.StatusActive)
	seedChat(t, 2, housechat.StatusPending)
	seedMember(t, 1, 7)
	seedMember(t, 2, 7)
	seedMember(t, 1, 8)
	require.NoError(t, repo.SetStatus(ctx, 1, 8, membership.StatusLeft))
	require.NoError(t, repo.SetForwardToDM(ctx, 1, 7, true))

	m, err := repo.GetMembership(ctx, 1, 7)
	require.NoError(t, err)
	assert.True(t, m.ForwardToDM)
	assert.True(t, m.NotifyMaterials, "уведомления о комментариях по умолчанию включены")
	assert.Equal(t, membership.StatusActive, m.Status)

	require.NoError(t, repo.SetNotifyMaterials(ctx, 1, 7, false))
	m, err = repo.GetMembership(ctx, 1, 7)
	require.NoError(t, err)
	assert.False(t, m.NotifyMaterials)
	assert.True(t, m.ForwardToDM, "настройки независимы")

	for _, tc := range []struct {
		name         string
		chat, userID int64
	}{
		{"чат pending", 2, 7},
		{"вышел из чата", 1, 8},
		{"не участник", 1, 999},
	} {
		_, err := repo.GetMembership(ctx, tc.chat, tc.userID)
		require.ErrorIs(t, err, usecase.ErrNotFound, tc.name)
	}

	// GetActiveForUpdate в транзакции возвращает то же и блокирует строку.
	err = NewTxManager(testPool).Do(ctx, func(ctx context.Context) error {
		m, err := repo.GetActiveForUpdate(ctx, 1, 7)
		assert.Equal(t, int64(7), m.UserID)
		return err
	})
	require.NoError(t, err)
}

func TestMembershipRepo_ForwardRecipients(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	repo := NewMembershipRepo(testPool)
	seedChat(t, 1, housechat.StatusActive)
	for _, uid := range []int64{1, 2, 3, 4, 5} {
		seedMember(t, 1, uid)
	}
	for _, uid := range []int64{1, 2, 4, 5} {
		require.NoError(t, repo.SetForwardToDM(ctx, 1, uid, true))
	}
	require.NoError(t, repo.MarkDMUnavailable(ctx, 4))
	require.NoError(t, repo.SetStatus(ctx, 1, 5, membership.StatusLeft))

	ids, err := repo.ForwardRecipients(ctx, 1, 1)
	require.NoError(t, err)
	assert.Equal(t, []int64{2}, ids, "без автора, без выключенной пересылки, недоступных и вышедших")
}

// --- TxManager ---

func TestTxManager_CommitAndRollback(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	tx := NewTxManager(testPool)
	chats := NewChatRepo(testPool)

	boom := errors.New("boom")
	err := tx.Do(ctx, func(ctx context.Context) error {
		require.NoError(t, chats.Upsert(ctx, housechat.Chat{ID: 1, Status: housechat.StatusActive}))
		return boom
	})
	require.ErrorIs(t, err, boom)
	_, err = chats.Get(ctx, 1)
	require.ErrorIs(t, err, usecase.ErrNotFound, "ошибка в fn откатывает транзакцию")

	require.NoError(t, tx.Do(ctx, func(ctx context.Context) error {
		return chats.Upsert(ctx, housechat.Chat{ID: 1, Status: housechat.StatusActive})
	}))
	_, err = chats.Get(ctx, 1)
	assert.NoError(t, err, "успешная fn коммитится")
}

func TestTxManager_RepositoriesShareTransaction(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	tx := NewTxManager(testPool)

	err := tx.Do(ctx, func(ctx context.Context) error {
		require.NoError(t, NewChatRepo(testPool).Upsert(ctx, housechat.Chat{ID: 1, Status: housechat.StatusActive}))
		// Участие ссылается на чат внешним ключом: видно только внутри той же транзакции.
		require.NoError(t, NewMembershipRepo(testPool).UpsertUser(ctx, membership.User{ID: 7}))
		return NewMembershipRepo(testPool).UpsertMembership(ctx, membership.Membership{
			ChatID: 1, UserID: 7, Role: membership.RoleMember, Status: membership.StatusActive,
		})
	})
	require.NoError(t, err)
}
