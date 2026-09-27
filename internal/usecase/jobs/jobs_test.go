package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/request"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

type fakeTx struct{}

func (fakeTx) Do(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

// fakeRequests воспроизводит выборки планировщика по статусам и срокам.
type fakeRequests struct {
	reqs    map[int64]request.Request
	saveErr error
}

func newFakeRequests(reqs ...request.Request) *fakeRequests {
	f := &fakeRequests{reqs: map[int64]request.Request{}}
	for _, r := range reqs {
		f.reqs[r.ID] = r
	}
	return f
}

func (f *fakeRequests) next(match func(request.Request) bool) (request.Request, bool, error) {
	var ids []int64
	for id, r := range f.reqs {
		if match(r) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return request.Request{}, false, nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return f.reqs[ids[0]], true, nil
}

func (f *fakeRequests) NextVotingEnded(_ context.Context, now time.Time) (request.Request, bool, error) {
	return f.next(func(r request.Request) bool {
		return r.Status == request.StatusOpen && !r.VotingEndsAt.After(now) && r.ExpiresAt.After(now)
	})
}

func (f *fakeRequests) NextExpired(_ context.Context, now time.Time) (request.Request, bool, error) {
	return f.next(func(r request.Request) bool { return r.IsActive() && !r.ExpiresAt.After(now) })
}

func (f *fakeRequests) SaveState(_ context.Context, r request.Request) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.reqs[r.ID] = r
	return nil
}

func (f *fakeRequests) UpdateBody(context.Context, int64, string) error { return nil }

func (f *fakeRequests) ListByAuthor(context.Context, int64, int64, int) ([]request.Request, error) {
	return nil, nil
}

type mockRater struct{ applied []request.Request }

func (m *mockRater) Apply(_ context.Context, r request.Request) (int, error) {
	m.applied = append(m.applied, r)
	return 0, nil
}

type mockSync struct{ synced []int64 }

func (m *mockSync) Sync(_ context.Context, id int64) bool {
	m.synced = append(m.synced, id)
	return true
}

var now = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func req(id int64, status request.Status, votingEnds, expires time.Duration) request.Request {
	return request.Request{
		ID: id, ChatID: 100, AuthorID: 1, Status: status,
		VotingEndsAt: now.Add(votingEnds), ExpiresAt: now.Add(expires),
	}
}

type lifecycleFixture struct {
	reqs  *fakeRequests
	rater *mockRater
	sync  *mockSync
	job   *Lifecycle
}

func newLifecycleFixture(batch int, reqs ...request.Request) *lifecycleFixture {
	f := &lifecycleFixture{reqs: newFakeRequests(reqs...), rater: &mockRater{}, sync: &mockSync{}}
	f.job = NewLifecycle(LifecycleDeps{
		Requests: f.reqs,
		Rater:    f.rater,
		Sync:     f.sync,
		Tx:       fakeTx{},
		Clock:    &fakeClock{now: now},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, batch)
	return f
}

func TestLifecycle_Run(t *testing.T) {
	f := newLifecycleFixture(100,
		req(1, request.StatusOpen, -time.Minute, time.Hour),          // голосование кончилось → in_progress
		req(2, request.StatusOpen, time.Minute, time.Hour),           // идёт голосование
		req(3, request.StatusOpen, -2*time.Hour, -time.Minute),       // истёк срок во время голосования → expired
		req(4, request.StatusInProgress, -2*time.Hour, -time.Second), // истёк срок → expired
		req(5, request.StatusInProgress, -2*time.Hour, time.Hour),    // ждёт автора
		req(6, request.StatusClosed, -2*time.Hour, -time.Hour),       // закрыта — не трогаем
		req(7, request.StatusDeleted, -2*time.Hour, -time.Hour),      // удалена — не трогаем
		req(8, request.StatusOpen, 0, time.Hour),                     // ровно в момент окончания
	)

	require.NoError(t, f.job.Run(context.Background()))

	want := map[int64]request.Status{
		1: request.StatusInProgress, 2: request.StatusOpen, 3: request.StatusExpired,
		4: request.StatusExpired, 5: request.StatusInProgress, 6: request.StatusClosed,
		7: request.StatusDeleted, 8: request.StatusInProgress,
	}
	for id, st := range want {
		assert.Equal(t, st, f.reqs.reqs[id].Status, "заявка %d", id)
	}
	assert.Equal(t, now, f.reqs.reqs[3].ClosedAt)

	var rated []int64
	for _, r := range f.rater.applied {
		rated = append(rated, r.ID)
	}
	assert.ElementsMatch(t, []int64{1, 3, 4, 8}, rated, "рейтинг при каждом завершении (идемпотентен)")
	assert.ElementsMatch(t, []int64{1, 3, 4, 8}, f.sync.synced, "сообщение в чате обновляется при каждом переходе")

	// Повторный проход ничего не меняет.
	f.sync.synced = nil
	require.NoError(t, f.job.Run(context.Background()))
	assert.Empty(t, f.sync.synced)
}

func TestLifecycle_Batch(t *testing.T) {
	f := newLifecycleFixture(2,
		req(1, request.StatusOpen, -time.Minute, time.Hour),
		req(2, request.StatusOpen, -time.Minute, time.Hour),
		req(3, request.StatusOpen, -time.Minute, time.Hour),
	)

	n, err := f.job.EndVoting(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, n, "не больше SCHEDULER_BATCH_SIZE за тик")

	n, err = f.job.EndVoting(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestLifecycle_SaveErrorStopsWithoutSync(t *testing.T) {
	f := newLifecycleFixture(10, req(1, request.StatusOpen, -time.Minute, time.Hour))
	f.reqs.saveErr = errors.New("pg down")

	_, err := f.job.EndVoting(context.Background())
	require.Error(t, err)
	assert.Empty(t, f.sync.synced, "сообщение в чате не трогаем без коммита")
}

// --- Сброс рейтингов ---

type fakeRuns struct{ last map[string]time.Time }

func (f *fakeRuns) TryStart(_ context.Context, job string, notAfter, now time.Time) (bool, error) {
	if last, ok := f.last[job]; ok && last.After(notAfter) {
		return false, nil
	}
	f.last[job] = now
	return true, nil
}

type fakeReset struct{ calls int }

func (f *fakeReset) ResetNegative(context.Context) (int, error) {
	f.calls++
	return 2, nil
}

func TestRatingReset_OncePerInterval(t *testing.T) {
	clock := &fakeClock{now: now}
	runs, resets := &fakeRuns{last: map[string]time.Time{}}, &fakeReset{}
	job := NewRatingReset(runs, resets, 14*24*time.Hour, fakeTx{}, clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	require.NoError(t, job.Run(ctx))
	assert.Equal(t, 1, resets.calls, "первый запуск сбрасывает сразу")

	// Каждый тик планировщика вызывает задачу, но сброса нет.
	for _, d := range []time.Duration{time.Minute, 7 * 24 * time.Hour, 14*24*time.Hour - time.Second} {
		clock.now = now.Add(d)
		require.NoError(t, job.Run(ctx))
	}
	assert.Equal(t, 1, resets.calls, "до истечения интервала повторно не сбрасываем")

	clock.now = now.Add(14 * 24 * time.Hour)
	require.NoError(t, job.Run(ctx))
	assert.Equal(t, 2, resets.calls, "ровно через интервал — сброс")

	clock.now = clock.now.Add(time.Hour)
	require.NoError(t, job.Run(ctx))
	assert.Equal(t, 2, resets.calls, "интервал отсчитывается от последнего сброса")
}
