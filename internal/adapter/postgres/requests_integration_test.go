//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/comment"
	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/domain/vote"
	"maxhouse/internal/usecase"
)

// seedHouse — активный чат 1 с автором 1 и соседями 2, 3.
func seedHouse(t *testing.T) {
	t.Helper()
	resetDB(t)
	seedChat(t, 1, housechat.StatusActive)
	seedUser(t, 1, "автор")
	for _, uid := range []int64{1, 2, 3} {
		seedMember(t, 1, uid)
	}
}

func TestRequestRepo_CreateAndGet(t *testing.T) {
	seedHouse(t)
	ctx := context.Background()
	repo := NewRequestRepo(testPool)
	photo := request.Attachment{Kind: request.KindImage, Ref: "tok1"}
	video := request.Attachment{Kind: request.KindVideo, Ref: "tok2"}

	id := seedRequest(t, request.Request{
		ChatID: 1, AuthorID: 1, Type: requesttype.Poll, IsAnonymous: true, Body: "Шлагбаум?",
		CreatedAt: t0, VotingEndsAt: t0.Add(time.Hour), ExpiresAt: t0.Add(24 * time.Hour),
	}, photo, video)
	assert.Equal(t, int64(1), id)

	var got request.Request
	err := NewTxManager(testPool).Do(ctx, func(ctx context.Context) error {
		var err error
		got, err = repo.GetForUpdate(ctx, id)
		return err
	})
	require.NoError(t, err)
	assert.Equal(t, requesttype.Poll, got.Type)
	assert.Equal(t, request.StatusOpen, got.Status)
	assert.True(t, got.IsAnonymous)
	assert.Equal(t, "Шлагбаум?", got.Body)
	assert.True(t, t0.Equal(got.CreatedAt))
	assert.True(t, t0.Add(time.Hour).Equal(got.VotingEndsAt))
	assert.True(t, t0.Add(24*time.Hour).Equal(got.ExpiresAt))
	assert.True(t, got.ClosedAt.IsZero(), "NULL closed_at — нулевое время")
	assert.Empty(t, got.ChatMessageID)

	_, err = repo.GetForUpdate(ctx, 404)
	require.ErrorIs(t, err, usecase.ErrNotFound)

	card, err := repo.LoadCard(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, []request.Attachment{photo, video}, card.Attachments, "вложения в порядке добавления")
}

func TestRequestRepo_CountCreatedSince(t *testing.T) {
	seedHouse(t)
	ctx := context.Background()
	repo := NewRequestRepo(testPool)

	seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, CreatedAt: t0.Add(-25 * time.Hour)})
	seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, CreatedAt: t0.Add(-24 * time.Hour)})
	deleted := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, CreatedAt: t0.Add(-time.Hour)})
	setRequestStatus(t, deleted, request.StatusDeleted)
	seedRequest(t, request.Request{ChatID: 1, AuthorID: 2, CreatedAt: t0})
	seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, Type: requesttype.Poll, CreatedAt: t0})

	n, err := repo.CountCreatedSince(ctx, 1, 1, requesttype.Complaint, t0.Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 2, n, "граница окна включительно, удалённые считаются, чужие и другие типы — нет")

	n, err = repo.CountCreatedSince(ctx, 1, 1, requesttype.Poll, t0.Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n, "лимит считается по каждому типу отдельно")

	// Тот же житель в другом доме: лимит свой в каждом чате.
	seedChat(t, 2, housechat.StatusActive)
	seedMember(t, 2, 1)
	seedRequest(t, request.Request{ChatID: 2, AuthorID: 1, CreatedAt: t0})
	n, err = repo.CountCreatedSince(ctx, 1, 1, requesttype.Complaint, t0.Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 2, n, "заявки в другом доме не влияют на лимит в этом")
	n, err = repo.CountCreatedSince(ctx, 2, 1, requesttype.Complaint, t0.Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n, "в другом доме свой счёт")
}

func TestRequestRepo_ChatSync(t *testing.T) {
	seedHouse(t)
	ctx := context.Background()
	repo := NewRequestRepo(testPool)
	id := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1})

	syncState := func() (string, string) {
		var mid *string
		var state string
		require.NoError(t, testPool.QueryRow(ctx,
			`SELECT chat_message_id, chat_sync FROM requests WHERE id = $1`, id).Scan(&mid, &state))
		if mid == nil {
			return "", state
		}
		return *mid, state
	}

	require.NoError(t, repo.SetChatSyncFailed(ctx, id))
	mid, state := syncState()
	assert.Empty(t, mid)
	assert.Equal(t, "failed", state)

	require.NoError(t, repo.SetChatMessage(ctx, id, "mid.1"))
	mid, state = syncState()
	assert.Equal(t, "mid.1", mid)
	assert.Equal(t, "ok", state)
}

func TestRequestRepo_ClaimRatingOnce(t *testing.T) {
	seedHouse(t)
	ctx := context.Background()
	repo := NewRequestRepo(testPool)
	id := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1})

	first, err := repo.ClaimRating(ctx, id)
	require.NoError(t, err)
	second, err := repo.ClaimRating(ctx, id)
	require.NoError(t, err)
	assert.True(t, first)
	assert.False(t, second, "рейтинг по заявке считается один раз")
}

func TestRequestRepo_LoadCard(t *testing.T) {
	seedHouse(t)
	ctx := context.Background()
	repo := NewRequestRepo(testPool)
	votes := NewVoteRepo(testPool)
	photo := request.Attachment{Kind: request.KindImage, Ref: "req-photo"}

	public := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1}, photo)
	anon := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, IsAnonymous: true})

	_, err := votes.Upsert(ctx, public, 2, vote.Confirm)
	require.NoError(t, err)
	_, err = votes.Upsert(ctx, public, 3, vote.Refute)
	require.NoError(t, err)
	_, err = NewCommentRepo(testPool).Add(ctx, comment.Comment{
		RequestID: public, AuthorID: 2, Kind: comment.KindEvidence, CreatedAt: t0,
		Attachments: []request.Attachment{{Kind: request.KindFile, Ref: "comment-file"}},
	})
	require.NoError(t, err)

	card, err := repo.LoadCard(ctx, public)
	require.NoError(t, err)
	assert.Equal(t, "автор", card.AuthorNickname)
	assert.Equal(t, 1, card.Confirms)
	assert.Equal(t, 1, card.Refutes)
	assert.Equal(t, 1, card.Materials)
	assert.Equal(t, []request.Attachment{photo}, card.Attachments, "вложения материалов в карточку не попадают")

	card, err = repo.LoadCard(ctx, anon)
	require.NoError(t, err)
	assert.Empty(t, card.AuthorNickname, "ник автора анонимной заявки не читается")
	assert.Equal(t, int64(1), card.Request.AuthorID, "автор известен системе")

	_, err = repo.LoadCard(ctx, 404)
	assert.ErrorIs(t, err, usecase.ErrNotFound)
}

func TestRequestRepo_SaveStateAndUpdateBody(t *testing.T) {
	seedHouse(t)
	ctx := context.Background()
	repo := NewRequestRepo(testPool)
	id := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1})

	closed := request.Request{ID: id, Status: request.StatusClosed, ResultText: "Решено", ClosedAt: t0.Add(time.Hour)}
	require.NoError(t, repo.SaveState(ctx, closed))
	require.NoError(t, repo.UpdateBody(ctx, id, "новый текст"))

	card, err := repo.LoadCard(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, request.StatusClosed, card.Request.Status)
	assert.Equal(t, "Решено", card.Request.ResultText)
	assert.True(t, t0.Add(time.Hour).Equal(card.Request.ClosedAt))
	assert.Equal(t, "новый текст", card.Request.Body)

	// Пустой итог и нулевое время сохраняются как NULL.
	require.NoError(t, repo.SaveState(ctx, request.Request{ID: id, Status: request.StatusDeleted}))
	var result, closedAt *string
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT result_text, closed_at::text FROM requests WHERE id = $1`, id).Scan(&result, &closedAt))
	assert.Nil(t, result)
	assert.Nil(t, closedAt)
}

func TestRequestRepo_Lists(t *testing.T) {
	seedHouse(t)
	ctx := context.Background()
	repo := NewRequestRepo(testPool)

	r1 := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, CreatedAt: t0})
	r2 := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, CreatedAt: t0.Add(time.Hour)})
	r3 := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, CreatedAt: t0.Add(2 * time.Hour)})
	r4 := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, CreatedAt: t0.Add(3 * time.Hour)})
	other := seedRequest(t, request.Request{ChatID: 1, AuthorID: 2, CreatedAt: t0.Add(4 * time.Hour)})
	setRequestStatus(t, r2, request.StatusClosed)
	setRequestStatus(t, r3, request.StatusDeleted)
	setRequestStatus(t, r4, request.StatusInProgress)

	mine, err := repo.ListByAuthor(ctx, 1, 1, 10)
	require.NoError(t, err)
	assert.Equal(t, []int64{r4, r2, r1}, ids(mine), "свои, без удалённых, новые первыми")

	mine, err = repo.ListByAuthor(ctx, 1, 1, 2)
	require.NoError(t, err)
	assert.Equal(t, []int64{r4, r2}, ids(mine), "лимит")

	house, err := repo.ListActiveInChat(ctx, 1, 10)
	require.NoError(t, err)
	assert.Equal(t, []int64{other, r4, r1}, ids(house), "только open и in_progress")
}

func ids(list []request.Request) []int64 {
	out := make([]int64, 0, len(list))
	for _, r := range list {
		out = append(out, r.ID)
	}
	return out
}

// --- Выборки планировщика ---

func TestRequestRepo_SchedulerSelections(t *testing.T) {
	seedHouse(t)
	ctx := context.Background()
	repo := NewRequestRepo(testPool)
	now := t0

	votingEnded := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, VotingEndsAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)})
	exactlyNow := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, VotingEndsAt: now, ExpiresAt: now.Add(time.Hour)})
	seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, VotingEndsAt: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour)})
	expiredOpen := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, VotingEndsAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)})
	expiredInProgress := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, VotingEndsAt: now.Add(-3 * time.Hour), ExpiresAt: now})
	setRequestStatus(t, expiredInProgress, request.StatusInProgress)
	closedExpired := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, VotingEndsAt: now.Add(-3 * time.Hour), ExpiresAt: now.Add(-time.Hour)})
	setRequestStatus(t, closedExpired, request.StatusClosed)

	drain := func(next func(context.Context, time.Time) (request.Request, bool, error), status request.Status) []int64 {
		var got []int64
		for {
			var r request.Request
			var found bool
			err := NewTxManager(testPool).Do(ctx, func(ctx context.Context) error {
				var err error
				if r, found, err = next(ctx, now); err != nil || !found {
					return err
				}
				r.Status = status
				return repo.SaveState(ctx, r)
			})
			require.NoError(t, err)
			if !found {
				return got
			}
			got = append(got, r.ID)
		}
	}

	assert.Equal(t, []int64{expiredOpen, expiredInProgress}, drain(repo.NextExpired, request.StatusExpired),
		"open и in_progress с expires_at <= now, по сроку; закрытые не трогаем")
	assert.Equal(t, []int64{votingEnded, exactlyNow}, drain(repo.NextVotingEnded, request.StatusInProgress),
		"open с voting_ends_at <= now и неистёкшим сроком")
}

func TestRequestRepo_SkipLocked(t *testing.T) {
	seedHouse(t)
	ctx := context.Background()
	repo := NewRequestRepo(testPool)
	first := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, VotingEndsAt: t0.Add(-2 * time.Minute), ExpiresAt: t0.Add(time.Hour)})
	second := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1, VotingEndsAt: t0.Add(-time.Minute), ExpiresAt: t0.Add(time.Hour)})

	locked := make(chan int64)
	release := make(chan struct{})
	done := make(chan error)
	go func() {
		done <- NewTxManager(testPool).Do(ctx, func(ctx context.Context) error {
			r, _, err := repo.NextVotingEnded(ctx, t0)
			locked <- r.ID
			<-release
			return err
		})
	}()
	assert.Equal(t, first, <-locked, "первая реплика взяла самую раннюю заявку")

	// Вторая реплика, пока первая держит блокировку.
	var got []int64
	err := NewTxManager(testPool).Do(ctx, func(ctx context.Context) error {
		r, found, err := repo.NextVotingEnded(ctx, t0)
		if found {
			got = append(got, r.ID)
		}
		return err
	})
	require.NoError(t, err)
	close(release)
	require.NoError(t, <-done)
	assert.Equal(t, []int64{second}, got, "занятая строка пропускается, а не ожидается")
}
