//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/broadcast"
	"maxhouse/internal/domain/comment"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/rating"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/vote"
)

// --- VoteRepo ---

func TestVoteRepo_UpsertReturnsPrevious(t *testing.T) {
	seedHouse(t)
	ctx := context.Background()
	votes := NewVoteRepo(testPool)
	id := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1})

	steps := []struct {
		value vote.Value
		prev  vote.Value
	}{
		{vote.Confirm, ""},           // первый голос
		{vote.Confirm, vote.Confirm}, // повтор той же кнопки
		{vote.Refute, vote.Confirm},  // смена голоса
		{vote.Refute, vote.Refute},
	}
	for i, s := range steps {
		prev, err := votes.Upsert(ctx, id, 2, s.value)
		require.NoError(t, err, i)
		assert.Equal(t, s.prev, prev, "шаг %d", i)
	}

	_, err := votes.Upsert(ctx, id, 3, vote.Refute)
	require.NoError(t, err)
	c, r, err := votes.Count(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, 0, c)
	assert.Equal(t, 2, r, "один голос на человека")
}

// --- RatingRepo ---

func membershipRating(t *testing.T, chatID, userID int64) int {
	t.Helper()
	var r int
	require.NoError(t, testPool.QueryRow(context.Background(),
		`SELECT rating FROM memberships WHERE chat_id = $1 AND user_id = $2`, chatID, userID).Scan(&r))
	return r
}

func TestRatingRepo_AddEventUpdatesCache(t *testing.T) {
	seedHouse(t)
	ctx := context.Background()
	repo := NewRatingRepo(testPool)
	id := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1})

	err := NewTxManager(testPool).Do(ctx, func(ctx context.Context) error {
		if err := repo.AddEvent(ctx, rating.Event{ChatID: 1, UserID: 1, RequestID: id, Delta: 1, Reason: rating.ReasonConfirmed}); err != nil {
			return err
		}
		return repo.AddEvent(ctx, rating.Event{ChatID: 1, UserID: 1, RequestID: id, Delta: -3, Reason: rating.ReasonRefuted})
	})
	require.NoError(t, err)

	assert.Equal(t, -2, membershipRating(t, 1, 1), "memberships.rating — сумма событий")
	var sum, n int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT coalesce(sum(delta), 0), count(*) FROM rating_events WHERE chat_id = 1 AND user_id = 1`).Scan(&sum, &n))
	assert.Equal(t, -2, sum)
	assert.Equal(t, 2, n, "каждое изменение — событие")
}

func TestRatingRepo_ResetNegative(t *testing.T) {
	seedHouse(t)
	ctx := context.Background()
	repo := NewRatingRepo(testPool)
	_, err := testPool.Exec(ctx, `UPDATE memberships SET rating = CASE user_id WHEN 1 THEN -4 WHEN 2 THEN 3 ELSE 0 END`)
	require.NoError(t, err)

	n, err := repo.ResetNegative(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 0, membershipRating(t, 1, 1))
	assert.Equal(t, 3, membershipRating(t, 1, 2), "положительный рейтинг не трогаем")

	var delta int
	var reason string
	var requestID *int64
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT delta, reason, request_id FROM rating_events WHERE user_id = 1`).Scan(&delta, &reason, &requestID))
	assert.Equal(t, 4, delta)
	assert.Equal(t, "reset", reason)
	assert.Nil(t, requestID)

	n, err = repo.ResetNegative(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "повторный сброс ничего не меняет")
}

// --- JobRuns ---

func TestJobRuns_TryStartOncePerPeriod(t *testing.T) {
	resetDB(t)
	ctx := context.Background()
	runs := NewJobRuns(testPool)
	week := t0.Add(-7 * 24 * time.Hour) // граница: «сейчас минус интервал»

	try := func(notAfter, now time.Time) bool {
		var ok bool
		err := NewTxManager(testPool).Do(ctx, func(ctx context.Context) error {
			var err error
			ok, err = runs.TryStart(ctx, "rating_reset", notAfter, now)
			return err
		})
		require.NoError(t, err)
		return ok
	}

	assert.True(t, try(week, t0), "первый запуск")
	assert.False(t, try(week, t0.Add(time.Hour)), "интервал не прошёл — нет")
	exactly := t0.Add(7 * 24 * time.Hour)
	assert.True(t, try(t0, exactly), "ровно через интервал — запуск (граница включительно)")
	assert.False(t, try(exactly.Add(-7*24*time.Hour).Add(time.Minute), exactly.Add(time.Minute)),
		"интервал считается от последнего запуска")
	assert.True(t, func() bool {
		var ok bool
		_ = NewTxManager(testPool).Do(ctx, func(ctx context.Context) error {
			var err error
			ok, err = runs.TryStart(ctx, "other_job", week, t0)
			return err
		})
		return ok
	}(), "задачи учитываются раздельно")
}

// --- CommentRepo ---

func TestCommentRepo_AddListCount(t *testing.T) {
	seedHouse(t)
	ctx := context.Background()
	repo := NewCommentRepo(testPool)
	id := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1})
	seedUser(t, 2, "сосед2")
	seedUser(t, 3, "сосед3")

	photo := request.Attachment{Kind: request.KindImage, Ref: "p"}
	video := request.Attachment{Kind: request.KindVideo, Ref: "v"}
	add := func(c comment.Comment) {
		c.RequestID = id
		err := NewTxManager(testPool).Do(ctx, func(ctx context.Context) error {
			_, err := repo.Add(ctx, c)
			return err
		})
		require.NoError(t, err)
	}
	add(comment.Comment{AuthorID: 2, Kind: comment.KindComment, Body: "первый", CreatedAt: t0})
	add(comment.Comment{AuthorID: 3, Kind: comment.KindEvidence, CreatedAt: t0.Add(time.Minute), Attachments: []request.Attachment{photo, video}})
	add(comment.Comment{AuthorID: 2, Kind: comment.KindComment, Body: "третий", CreatedAt: t0.Add(2 * time.Minute)})

	n, err := repo.Count(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	page, err := repo.List(ctx, id, 0, 2)
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, "первый", page[0].Body)
	assert.Equal(t, "сосед2", page[0].AuthorNickname, "автор заявки видит ник отправителя")
	assert.Empty(t, page[0].Attachments)
	assert.Equal(t, comment.KindEvidence, page[1].Kind)
	assert.Equal(t, "сосед3", page[1].AuthorNickname)
	assert.Equal(t, []request.Attachment{photo, video}, page[1].Attachments)
	assert.True(t, t0.Add(time.Minute).Equal(page[1].CreatedAt))

	page, err = repo.List(ctx, id, 2, 2)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, "третий", page[0].Body)

	page, err = repo.List(ctx, id, 10, 2)
	require.NoError(t, err)
	assert.Empty(t, page)
}

// --- BroadcastRepo ---

func TestBroadcastRepo(t *testing.T) {
	seedHouse(t)
	ctx := context.Background()
	repo := NewBroadcastRepo(testPool)
	members := NewMembershipRepo(testPool)
	votes := NewVoteRepo(testPool)
	id := seedRequest(t, request.Request{ChatID: 1, AuthorID: 1})

	for _, uid := range []int64{4, 5, 6} {
		seedMember(t, 1, uid)
	}
	cast := map[int64]vote.Value{2: vote.Confirm, 3: vote.Refute, 4: vote.Confirm, 5: vote.Confirm, 6: vote.Refute}
	for uid, v := range cast {
		_, err := votes.Upsert(ctx, id, uid, v)
		require.NoError(t, err)
	}
	require.NoError(t, members.MarkDMUnavailable(ctx, 4))                   // не начинал диалог
	require.NoError(t, members.SetStatus(ctx, 1, 5, membership.StatusLeft)) // вышел из чата

	for aud, want := range map[broadcast.Audience][]int64{
		broadcast.AudienceAll:     {2, 3, 6},
		broadcast.AudienceConfirm: {2},
		broadcast.AudienceRefute:  {3, 6},
	} {
		got, err := repo.Recipients(ctx, id, aud)
		require.NoError(t, err)
		assert.Equal(t, want, got, aud)
	}

	_, found, err := repo.LastAt(ctx, id)
	require.NoError(t, err)
	assert.False(t, found, "рассылок ещё не было")

	require.NoError(t, repo.Record(ctx, id, broadcast.AudienceAll, 3, t0))
	require.NoError(t, repo.Record(ctx, id, broadcast.AudienceRefute, 2, t0.Add(time.Hour)))
	last, found, err := repo.LastAt(ctx, id)
	require.NoError(t, err)
	assert.True(t, found)
	assert.True(t, t0.Add(time.Hour).Equal(last), "последняя рассылка")
}
