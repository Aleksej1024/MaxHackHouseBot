//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/problem"
)

func TestProblemRepo_CreateCurrentReporters(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	repo := NewProblemRepo(testPool)
	seedChat(t, 1, housechat.StatusActive)
	seedChat(t, 2, housechat.StatusActive)
	for _, uid := range []int64{10, 11, 12} {
		seedUser(t, uid, "житель")
	}
	openSince := t0.Add(-30 * time.Minute)

	_, found, err := repo.Current(ctx, 1, problem.NoWater, openSince)
	require.NoError(t, err)
	assert.False(t, found)

	id, err := repo.Create(ctx, problem.New(1, problem.NoWater, t0))
	require.NoError(t, err)
	// Другие проблема и дом не мешают.
	otherProblem, err := repo.Create(ctx, problem.New(1, problem.NoPower, t0))
	require.NoError(t, err)
	_, err = repo.Create(ctx, problem.New(2, problem.NoWater, t0))
	require.NoError(t, err)

	got, found, err := repo.Current(ctx, 1, problem.NoWater, openSince)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, id, got.ID)
	assert.Zero(t, got.Reporters, "окно без жителей")
	assert.False(t, got.Notified)
	assert.True(t, got.NotifiedAt.IsZero(), "NULL notified_at — нулевое время")
	assert.True(t, t0.Equal(got.FirstReportedAt))

	for i, uid := range []int64{10, 11, 12} {
		added, err := repo.AddReporter(ctx, id, uid, t0.Add(time.Duration(i)*time.Minute))
		require.NoError(t, err)
		assert.True(t, added)
	}
	added, err := repo.AddReporter(ctx, id, 11, t0.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, added, "житель в окне один раз")
	added, err = repo.AddReporter(ctx, otherProblem, 11, t0)
	require.NoError(t, err)
	assert.True(t, added, "в другом окне — отдельно")

	var reportedAt time.Time
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT reported_at FROM problem_report_users WHERE report_id = $1 AND user_id = 11`, id).Scan(&reportedAt))
	assert.True(t, t0.Add(time.Minute).Equal(reportedAt), "повтор не перезаписывает время")

	require.NoError(t, repo.SetNotified(ctx, id, t0.Add(5*time.Minute)))
	got, _, err = repo.Current(ctx, 1, problem.NoWater, openSince)
	require.NoError(t, err)
	assert.Equal(t, 3, got.Reporters)
	assert.True(t, got.Notified)
	assert.True(t, t0.Add(5*time.Minute).Equal(got.NotifiedAt))

	require.NoError(t, repo.SetNotified(ctx, id, time.Time{}))
	got, _, err = repo.Current(ctx, 1, problem.NoWater, openSince)
	require.NoError(t, err)
	assert.False(t, got.Notified, "отметка снята")
	assert.True(t, got.NotifiedAt.IsZero())

	// Окно начато ровно на границе — уже закрыто.
	_, found, err = repo.Current(ctx, 1, problem.NoWater, t0)
	require.NoError(t, err)
	assert.False(t, found, "first_reported_at > openSince: граница не включается")
}

func TestProblemRepo_LockSerializes(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	repo := NewProblemRepo(testPool)
	seedChat(t, 1, housechat.StatusActive)

	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error)
	go func() {
		done <- NewTxManager(testPool).Do(ctx, func(ctx context.Context) error {
			defer func() {
				select {
				case <-locked:
				default:
					close(locked) // ошибка до блокировки — не вешаем тест
				}
			}()
			if err := repo.Lock(ctx, 1, problem.NoGas); err != nil {
				return err
			}
			close(locked)
			<-release
			_, err := repo.Create(ctx, problem.New(1, problem.NoGas, t0))
			return err
		})
	}()
	<-locked
	select {
	case err := <-done:
		require.NoError(t, err, "первая транзакция завершилась раньше времени")
		t.FailNow()
	default:
	}

	// Другая проблема того же дома не ждёт.
	require.NoError(t, NewTxManager(testPool).Do(ctx, func(ctx context.Context) error {
		return repo.Lock(ctx, 1, problem.NoWater)
	}))

	// Та же проблема ждёт первую транзакцию и видит созданное ею окно.
	second := make(chan bool)
	go func() {
		_ = NewTxManager(testPool).Do(ctx, func(ctx context.Context) error {
			if err := repo.Lock(ctx, 1, problem.NoGas); err != nil {
				return err
			}
			_, found, err := repo.Current(ctx, 1, problem.NoGas, t0.Add(-time.Hour))
			second <- found
			return err
		})
	}()
	select {
	case <-second:
		t.Fatal("вторая транзакция не должна пройти блокировку раньше первой")
	default:
	}
	close(release)
	require.NoError(t, <-done)
	assert.True(t, <-second, "после снятия блокировки видно окно первой транзакции")
}

func TestMembershipRepo_ListChatAdmins(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	repo := NewMembershipRepo(testPool)
	seedChat(t, 1, housechat.StatusActive)
	roles := map[int64]membership.Role{1: membership.RoleAdmin, 2: membership.RoleMember, 3: membership.RoleAdmin}
	for _, uid := range []int64{1, 2, 3} {
		seedUser(t, uid, "житель")
		require.NoError(t, repo.UpsertMembership(ctx, membership.Membership{
			ChatID: 1, UserID: uid, Role: roles[uid], Status: membership.StatusActive,
		}))
	}
	require.NoError(t, repo.SetStatus(ctx, 1, 3, membership.StatusLeft))

	admins, err := repo.ListChatAdmins(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, []membership.User{{ID: 1, Nickname: "житель"}}, admins, "только активные админы")
}
