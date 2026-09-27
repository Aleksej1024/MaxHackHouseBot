package voting

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/rating"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/domain/vote"
	"maxhouse/internal/usecase"
)

// --- Тестовые дублёры ---

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

type fakeTx struct{}

func (fakeTx) Do(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

// fakeRequests — in-memory заявки с флагом rating_applied.
type fakeRequests struct {
	reqs    map[int64]request.Request
	applied map[int64]bool
	locked  []int64
	err     error
}

func newFakeRequests(reqs ...request.Request) *fakeRequests {
	f := &fakeRequests{reqs: map[int64]request.Request{}, applied: map[int64]bool{}}
	for _, r := range reqs {
		f.reqs[r.ID] = r
	}
	return f
}

func (f *fakeRequests) GetForUpdate(_ context.Context, id int64) (request.Request, error) {
	if f.err != nil {
		return request.Request{}, f.err
	}
	f.locked = append(f.locked, id)
	r, ok := f.reqs[id]
	if !ok {
		return request.Request{}, usecase.ErrNotFound
	}
	return r, nil
}

func (f *fakeRequests) ClaimRating(_ context.Context, id int64) (bool, error) {
	if f.applied[id] {
		return false, nil
	}
	f.applied[id] = true
	return true, nil
}

// stubMembers — участники чатов: key = chatID<<32 | userID не нужен, храним пары.
type stubMembers struct {
	active map[[2]int64]bool
}

func (s stubMembers) GetActiveForUpdate(_ context.Context, chatID, userID int64) (membership.Membership, error) {
	if !s.active[[2]int64{chatID, userID}] {
		return membership.Membership{}, usecase.ErrNotFound
	}
	return membership.Membership{ChatID: chatID, UserID: userID, Status: membership.StatusActive}, nil
}

type fakeVotes struct {
	votes map[int64]map[int64]vote.Value
	err   error
}

func newFakeVotes() *fakeVotes { return &fakeVotes{votes: map[int64]map[int64]vote.Value{}} }

func (f *fakeVotes) Upsert(_ context.Context, requestID, userID int64, value vote.Value) (vote.Value, error) {
	if f.err != nil {
		return "", f.err
	}
	if f.votes[requestID] == nil {
		f.votes[requestID] = map[int64]vote.Value{}
	}
	prev := f.votes[requestID][userID]
	f.votes[requestID][userID] = value
	return prev, nil
}

func (f *fakeVotes) Count(_ context.Context, requestID int64) (int, int, error) {
	var c, r int
	for _, v := range f.votes[requestID] {
		if v == vote.Confirm {
			c++
		} else {
			r++
		}
	}
	return c, r, nil
}

// mockSync запоминает, какие заявки синхронизировались с чатом.
type mockSync struct{ synced []int64 }

func (m *mockSync) Sync(_ context.Context, id int64) bool {
	m.synced = append(m.synced, id)
	return true
}

type fakeRatings struct {
	events []rating.Event
	sum    map[[2]int64]int
}

func newFakeRatings() *fakeRatings { return &fakeRatings{sum: map[[2]int64]int{}} }

func (f *fakeRatings) AddEvent(_ context.Context, e rating.Event) error {
	f.events = append(f.events, e)
	f.sum[[2]int64{e.ChatID, e.UserID}] += e.Delta
	return nil
}

// --- Сборка ---

const (
	chatID   = int64(100)
	authorID = int64(1)
	voterID  = int64(2)
	otherID  = int64(3)
	reqID    = int64(10)
)

var now = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func openRequest() request.Request {
	return request.Request{
		ID: reqID, ChatID: chatID, AuthorID: authorID, Type: requesttype.Complaint,
		Status: request.StatusOpen, VotingEndsAt: now.Add(time.Hour), ExpiresAt: now.Add(24 * time.Hour),
	}
}

type fixture struct {
	reqs  *fakeRequests
	votes *fakeVotes
	sync  *mockSync
	svc   *Service
}

func newFixture(reqs ...request.Request) *fixture {
	f := &fixture{reqs: newFakeRequests(reqs...), votes: newFakeVotes(), sync: &mockSync{}}
	members := stubMembers{active: map[[2]int64]bool{
		{chatID, authorID}: true,
		{chatID, voterID}:  true,
	}}
	f.svc = New(Deps{
		Requests: f.reqs,
		Members:  members,
		Votes:    f.votes,
		Sync:     f.sync,
		Tx:       fakeTx{},
		Clock:    &fakeClock{now: now},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return f
}

// --- Vote ---

func TestVote_Accepted(t *testing.T) {
	f := newFixture(openRequest())

	res, err := f.svc.Vote(context.Background(), reqID, voterID, vote.Confirm)
	require.NoError(t, err)
	assert.Equal(t, Result{Outcome: OutcomeAccepted, Value: vote.Confirm, Type: requesttype.Complaint}, res)
	assert.Equal(t, vote.Confirm, f.votes.votes[reqID][voterID])
	assert.Equal(t, []int64{reqID}, f.sync.synced, "счётчики в чате обновляются")
	assert.Equal(t, []int64{reqID}, f.reqs.locked, "заявка блокируется на время голоса")
}

func TestVote_ChangeVote(t *testing.T) {
	f := newFixture(openRequest())
	ctx := context.Background()

	_, err := f.svc.Vote(ctx, reqID, voterID, vote.Confirm)
	require.NoError(t, err)
	res, err := f.svc.Vote(ctx, reqID, voterID, vote.Refute)
	require.NoError(t, err)

	assert.Equal(t, OutcomeAccepted, res.Outcome)
	assert.Equal(t, vote.Refute, f.votes.votes[reqID][voterID])
	c, r, _ := f.votes.Count(ctx, reqID)
	assert.Equal(t, [2]int{0, 1}, [2]int{c, r}, "один голос на человека")
	assert.Len(t, f.sync.synced, 2)
}

func TestVote_RepeatedIsIdempotent(t *testing.T) {
	f := newFixture(openRequest())
	ctx := context.Background()

	_, err := f.svc.Vote(ctx, reqID, voterID, vote.Confirm)
	require.NoError(t, err)
	res, err := f.svc.Vote(ctx, reqID, voterID, vote.Confirm)
	require.NoError(t, err)

	assert.Equal(t, OutcomeUnchanged, res.Outcome)
	assert.Equal(t, vote.Confirm, res.Value)
	assert.Len(t, f.sync.synced, 1, "повтор не трогает сообщение в чате")
}

func TestVote_Rejected(t *testing.T) {
	deleted := openRequest()
	deleted.Status = request.StatusDeleted
	inProgress := openRequest()
	inProgress.Status = request.StatusInProgress
	closed := openRequest()
	closed.Status = request.StatusClosed
	timeIsUp := openRequest()
	timeIsUp.VotingEndsAt = now // воркер ещё не перевёл в in_progress

	tests := []struct {
		name  string
		req   *request.Request
		voter int64
		want  Outcome
	}{
		{name: "автор за свою заявку", req: ptr(openRequest()), voter: authorID, want: OutcomeOwnRequest},
		{name: "не участник чата", req: ptr(openRequest()), voter: otherID, want: OutcomeNotMember},
		{name: "заявки нет", req: nil, voter: voterID, want: OutcomeNotFound},
		{name: "удалена", req: &deleted, voter: voterID, want: OutcomeNotFound},
		{name: "голосование завершено", req: &inProgress, voter: voterID, want: OutcomeClosed},
		{name: "заявка закрыта", req: &closed, voter: voterID, want: OutcomeClosed},
		{name: "время голосования вышло", req: &timeIsUp, voter: voterID, want: OutcomeClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f *fixture
			if tt.req != nil {
				f = newFixture(*tt.req)
			} else {
				f = newFixture()
			}

			res, err := f.svc.Vote(context.Background(), reqID, tt.voter, vote.Confirm)
			require.NoError(t, err)
			assert.Equal(t, tt.want, res.Outcome)
			assert.Empty(t, f.votes.votes[reqID], "голос не записан")
			assert.Empty(t, f.sync.synced)
		})
	}
}

func TestVote_StorageErrors(t *testing.T) {
	f := newFixture(openRequest())
	f.votes.err = errors.New("pg down")
	_, err := f.svc.Vote(context.Background(), reqID, voterID, vote.Confirm)
	require.Error(t, err)
	assert.Empty(t, f.sync.synced)

	f = newFixture(openRequest())
	f.reqs.err = errors.New("pg down")
	_, err = f.svc.Vote(context.Background(), reqID, voterID, vote.Confirm)
	require.Error(t, err)
}

func ptr[T any](v T) *T { return &v }

// --- Rater ---

func newRater(reqs *fakeRequests, votes *fakeVotes, ratings *fakeRatings, appliesTo string) *Rater {
	applies, err := rating.ParseAppliesTo(appliesTo)
	if err != nil {
		panic(err)
	}
	policy := rating.Policy{MinVotes: 1, ConfirmShare: 0.5, RefuteShare: 0.5, DeltaConfirmed: 1, DeltaRefuted: 1, AppliesTo: applies}
	return NewRater(reqs, votes, ratings, policy, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRater_Apply(t *testing.T) {
	tests := []struct {
		name      string
		typ       requesttype.Code
		appliesTo string
		votes     []vote.Value
		wantDelta int
	}{
		{name: "подтверждена", typ: requesttype.Complaint, appliesTo: "all", votes: []vote.Value{vote.Confirm, vote.Confirm, vote.Refute}, wantDelta: 1},
		{name: "опровергнута", typ: requesttype.Complaint, appliesTo: "all", votes: []vote.Value{vote.Refute}, wantDelta: -1},
		{name: "нет голосов", typ: requesttype.Complaint, appliesTo: "all"},
		{name: "ничья", typ: requesttype.Complaint, appliesTo: "all", votes: []vote.Value{vote.Confirm, vote.Refute}},
		{name: "тип вне RATING_APPLIES_TO_TYPES", typ: requesttype.Poll, appliesTo: "complaint,initiative", votes: []vote.Value{vote.Confirm}},
		{name: "опрос влияет при all", typ: requesttype.Poll, appliesTo: "all", votes: []vote.Value{vote.Confirm}, wantDelta: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := openRequest()
			req.Type = tt.typ
			reqs, votes, ratings := newFakeRequests(req), newFakeVotes(), newFakeRatings()
			for i, v := range tt.votes {
				_, _ = votes.Upsert(context.Background(), reqID, int64(100+i), v)
			}
			r := newRater(reqs, votes, ratings, tt.appliesTo)

			delta, err := r.Apply(context.Background(), req)
			require.NoError(t, err)
			assert.Equal(t, tt.wantDelta, delta)
			assert.Equal(t, tt.wantDelta, ratings.sum[[2]int64{chatID, authorID}], "рейтинг в паре (автор, чат)")
			if tt.wantDelta == 0 {
				assert.Empty(t, ratings.events)
			} else {
				require.Len(t, ratings.events, 1, "каждое изменение пишется в RATING_EVENT")
				assert.Equal(t, reqID, ratings.events[0].RequestID)
			}
			assert.True(t, reqs.applied[reqID], "флаг ставится и при нейтральном итоге")
		})
	}
}

func TestRater_Idempotent(t *testing.T) {
	req := openRequest()
	reqs, votes, ratings := newFakeRequests(req), newFakeVotes(), newFakeRatings()
	_, _ = votes.Upsert(context.Background(), reqID, voterID, vote.Confirm)
	r := newRater(reqs, votes, ratings, "all")

	for i := 0; i < 3; i++ {
		_, err := r.Apply(context.Background(), req)
		require.NoError(t, err)
	}
	assert.Len(t, ratings.events, 1, "повторный пересчёт не меняет рейтинг")
	assert.Equal(t, 1, ratings.sum[[2]int64{chatID, authorID}])
}

func TestRater_AnonymousSameAsPublic(t *testing.T) {
	req := openRequest()
	req.IsAnonymous = true
	reqs, votes, ratings := newFakeRequests(req), newFakeVotes(), newFakeRatings()
	_, _ = votes.Upsert(context.Background(), reqID, voterID, vote.Refute)

	delta, err := newRater(reqs, votes, ratings, "all").Apply(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, -1, delta)
}

func TestRater_DeletedRequestSkipped(t *testing.T) {
	req := openRequest()
	req.Status = request.StatusDeleted
	reqs, votes, ratings := newFakeRequests(req), newFakeVotes(), newFakeRatings()
	_, _ = votes.Upsert(context.Background(), reqID, voterID, vote.Confirm)

	delta, err := newRater(reqs, votes, ratings, "all").Apply(context.Background(), req)
	require.NoError(t, err)
	assert.Zero(t, delta)
	assert.Empty(t, ratings.events)
	assert.False(t, reqs.applied[reqID])
}
