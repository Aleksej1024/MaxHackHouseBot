package requests

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
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/usecase"
	"maxhouse/internal/usecase/notify"
)

// --- Тестовые дублёры ---

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// fakeFSMStore — in-memory FSMStore с TTL по FakeClock.
type fakeFSMStore struct {
	clock   *fakeClock
	data    map[string]string
	expires map[string]time.Time
	err     error
}

func newFakeFSMStore(clock *fakeClock) *fakeFSMStore {
	return &fakeFSMStore{clock: clock, data: map[string]string{}, expires: map[string]time.Time{}}
}

func (s *fakeFSMStore) Get(_ context.Context, key string) (string, bool, error) {
	if s.err != nil {
		return "", false, s.err
	}
	v, ok := s.data[key]
	if !ok || !s.clock.now.Before(s.expires[key]) {
		return "", false, nil
	}
	return v, true, nil
}

func (s *fakeFSMStore) Set(_ context.Context, key, value string, ttl time.Duration) error {
	if s.err != nil {
		return s.err
	}
	s.data[key] = value
	s.expires[key] = s.clock.now.Add(ttl)
	return nil
}

func (s *fakeFSMStore) Delete(_ context.Context, key string) error {
	if s.err != nil {
		return s.err
	}
	delete(s.data, key)
	delete(s.expires, key)
	return nil
}

type stubActiveChats struct {
	chat housechat.Chat
	ok   bool
	err  error
}

func (s stubActiveChats) ActiveChat(context.Context, int64) (housechat.Chat, bool, error) {
	return s.chat, s.ok, s.err
}

type stubMembers struct {
	m      membership.Membership
	err    error
	locked int
}

func (s *stubMembers) GetActiveForUpdate(context.Context, int64, int64) (membership.Membership, error) {
	s.locked++
	return s.m, s.err
}

type fakeRequestRepo struct {
	clock      *fakeClock
	created    []request.Request
	atts       map[int64][]request.Attachment
	messageIDs map[int64]string
	failed     map[int64]bool
	createErr  error
}

func newFakeRequestRepo(clock *fakeClock) *fakeRequestRepo {
	return &fakeRequestRepo{
		clock:      clock,
		atts:       map[int64][]request.Attachment{},
		messageIDs: map[int64]string{},
		failed:     map[int64]bool{},
	}
}

func (r *fakeRequestRepo) CountCreatedSince(_ context.Context, chatID, authorID int64, typ requesttype.Code, since time.Time) (int, error) {
	n := 0
	for _, req := range r.created {
		if req.ChatID == chatID && req.AuthorID == authorID && req.Type == typ && !req.CreatedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

func (r *fakeRequestRepo) Create(_ context.Context, req request.Request, atts []request.Attachment) (int64, error) {
	if r.createErr != nil {
		return 0, r.createErr
	}
	req.ID = int64(len(r.created) + 1)
	r.created = append(r.created, req)
	r.atts[req.ID] = atts
	return req.ID, nil
}

func (r *fakeRequestRepo) SetChatMessage(_ context.Context, id int64, mid string) error {
	r.messageIDs[id] = mid
	r.failed[id] = false
	return nil
}

func (r *fakeRequestRepo) SetChatSyncFailed(_ context.Context, id int64) error {
	r.failed[id] = true
	return nil
}

// mockPublisher проверяет вызовы публикации.
type mockPublisher struct {
	cards []usecase.RequestCard
	err   error
}

func (p *mockPublisher) PublishRequest(_ context.Context, card usecase.RequestCard) (string, error) {
	p.cards = append(p.cards, card)
	if p.err != nil {
		return "", p.err
	}
	return "mid.pub", nil
}

func (p *mockPublisher) EditRequest(context.Context, usecase.RequestCard) error   { return nil }
func (p *mockPublisher) DeleteRequest(context.Context, string) error              { return nil }
func (p *mockPublisher) PublishResult(context.Context, usecase.RequestCard) error { return nil }

// mockForwarder запоминает пересланные заявки.
type mockForwarder struct{ forwarded []int64 }

func (m *mockForwarder) ForwardNew(_ context.Context, id int64) notify.Delivery {
	m.forwarded = append(m.forwarded, id)
	return notify.Delivery{}
}

type fakeTx struct{}

func (fakeTx) Do(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

// --- Сборка ---

const (
	userID = int64(7)
	chatID = int64(100)
	fsmTTL = 30 * time.Minute
)

var (
	author = membership.User{ID: userID, Nickname: "сосед7"}
	policy = request.CreatePolicy{DailyLimit: 3, MinRating: -3, MaxBodyLen: 20, MaxAttachments: 2}
	photo  = request.Attachment{Kind: request.KindImage, Ref: "tok1"}
	video  = request.Attachment{Kind: request.KindVideo, Ref: "tok2"}
	doc    = request.Attachment{Kind: request.KindFile, Ref: "tok3"}
)

type fixture struct {
	clock   *fakeClock
	store   *fakeFSMStore
	members *stubMembers
	repo    *fakeRequestRepo
	pub     *mockPublisher
	fwd     *mockForwarder
	svc     *Creation
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	f := &fixture{
		clock:   clock,
		store:   newFakeFSMStore(clock),
		members: &stubMembers{m: membership.Membership{ChatID: chatID, UserID: userID, Status: membership.StatusActive}},
		repo:    newFakeRequestRepo(clock),
		pub:     &mockPublisher{},
		fwd:     &mockForwarder{},
	}
	f.svc = NewCreation(Deps{
		Chats:     stubActiveChats{chat: housechat.Chat{ID: chatID, Status: housechat.StatusActive}, ok: true},
		Members:   f.members,
		Requests:  f.repo,
		Publisher: f.pub,
		Forward:   f.fwd,
		Store:     f.store,
		Tx:        fakeTx{},
		Clock:     clock,
		Types: requesttype.New(
			requesttype.Type{Code: requesttype.Complaint},
			requesttype.Type{Code: requesttype.Poll, VotingDuration: time.Hour, TTL: 24 * time.Hour},
		),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, Config{
		Policy:         policy,
		FSMTTL:         fsmTTL,
		VotingDuration: 72 * time.Hour,
		TTLDefault:     14 * 24 * time.Hour,
	})
	return f
}

// toConfirm проводит диалог до шага подтверждения.
func (f *fixture) toConfirm(t *testing.T, code requesttype.Code, anonymous bool) {
	t.Helper()
	ctx := context.Background()
	_, err := f.svc.Begin(ctx, userID)
	require.NoError(t, err)
	_, err = f.svc.ChooseType(ctx, userID, code)
	require.NoError(t, err)
	_, _, err = f.svc.HandleMessage(ctx, userID, "Курят в подъезде", []request.Attachment{photo})
	require.NoError(t, err)
	_, err = f.svc.FinishMedia(ctx, userID)
	require.NoError(t, err)
	s, err := f.svc.ChooseAnonymity(ctx, userID, anonymous)
	require.NoError(t, err)
	require.Equal(t, StepConfirm, s.Kind)
}

// --- Полный сценарий ---

func TestCreation_HappyPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	s, err := f.svc.Begin(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, StepChooseType, s.Kind)
	require.Len(t, s.Types, 2)

	s, err = f.svc.ChooseType(ctx, userID, requesttype.Complaint)
	require.NoError(t, err)
	assert.Equal(t, StepEnterText, s.Kind)
	assert.Equal(t, requesttype.Complaint, s.Draft.Type)

	s, handled, err := f.svc.HandleMessage(ctx, userID, "  Курят в подъезде  ", nil)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, StepAddMedia, s.Kind)
	assert.Equal(t, "Курят в подъезде", s.Draft.Body, "пробелы по краям обрезаются")

	s, _, err = f.svc.HandleMessage(ctx, userID, "", []request.Attachment{photo, video})
	require.NoError(t, err)
	assert.Equal(t, StepAddMedia, s.Kind)
	require.NoError(t, s.Problem)
	assert.Len(t, s.Draft.Attachments, 2)

	s, err = f.svc.FinishMedia(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, StepChooseAnonymity, s.Kind)

	s, err = f.svc.ChooseAnonymity(ctx, userID, false)
	require.NoError(t, err)
	assert.Equal(t, StepConfirm, s.Kind)

	s, err = f.svc.Publish(ctx, author)
	require.NoError(t, err)
	assert.Equal(t, StepPublished, s.Kind)
	assert.True(t, s.ChatSynced)
	assert.Equal(t, int64(1), s.Request.ID)
	assert.Equal(t, "mid.pub", s.Request.ChatMessageID)

	require.Len(t, f.repo.created, 1)
	req := f.repo.created[0]
	assert.Equal(t, chatID, req.ChatID)
	assert.Equal(t, userID, req.AuthorID)
	assert.Equal(t, request.StatusOpen, req.Status)
	assert.Equal(t, f.clock.now, req.CreatedAt)
	assert.Equal(t, f.clock.now.Add(72*time.Hour), req.VotingEndsAt)
	assert.Equal(t, f.clock.now.Add(14*24*time.Hour), req.ExpiresAt)
	assert.Equal(t, []request.Attachment{photo, video}, f.repo.atts[1])
	assert.Equal(t, "mid.pub", f.repo.messageIDs[1])

	require.Len(t, f.pub.cards, 1, "в чат публикуется ровно одно сообщение")
	assert.Equal(t, []int64{1}, f.fwd.forwarded, "новая заявка пересылается в личку")
	assert.Equal(t, "сосед7", f.pub.cards[0].AuthorNickname)
	assert.Equal(t, []request.Attachment{photo, video}, f.pub.cards[0].Attachments)

	_, found, _ := f.store.Get(ctx, "fsm:7")
	assert.False(t, found, "черновик удаляется после публикации")
}

func TestCreation_TypeDurationOverride(t *testing.T) {
	f := newFixture(t)
	f.toConfirm(t, requesttype.Poll, false)

	s, err := f.svc.Publish(context.Background(), author)
	require.NoError(t, err)
	assert.Equal(t, f.clock.now.Add(time.Hour), s.Request.VotingEndsAt)
	assert.Equal(t, f.clock.now.Add(24*time.Hour), s.Request.ExpiresAt)
}

func TestCreation_Anonymous(t *testing.T) {
	f := newFixture(t)
	f.toConfirm(t, requesttype.Complaint, true)

	_, err := f.svc.Publish(context.Background(), author)
	require.NoError(t, err)

	req := f.repo.created[0]
	assert.True(t, req.IsAnonymous)
	assert.Equal(t, userID, req.AuthorID, "автор хранится в БД всегда")
	require.Len(t, f.pub.cards, 1)
	assert.Empty(t, f.pub.cards[0].AuthorNickname, "ник анонимного автора не уходит в чат")
}

func TestCreation_EditKeepsAttachments(t *testing.T) {
	f := newFixture(t)
	f.toConfirm(t, requesttype.Complaint, false)
	ctx := context.Background()

	s, err := f.svc.Edit(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, StepEnterText, s.Kind)

	s, _, err = f.svc.HandleMessage(ctx, userID, "Новый текст", nil)
	require.NoError(t, err)
	assert.Equal(t, StepAddMedia, s.Kind)
	assert.Equal(t, "Новый текст", s.Draft.Body)
	assert.Equal(t, []request.Attachment{photo}, s.Draft.Attachments)
}

// --- Проверки ввода ---

func TestCreation_TextValidation(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		atts    []request.Attachment
		problem error
	}{
		{name: "пустой текст", text: "   ", problem: request.ErrEmptyBody},
		{name: "только вложение без текста", atts: []request.Attachment{photo}, problem: request.ErrEmptyBody},
		{name: "ровно на границе длины", text: "12345678901234567890"},
		{name: "длиннее границы", text: "123456789012345678901", problem: request.ErrBodyTooLong},
		{name: "текст с лишними вложениями", text: "ок", atts: []request.Attachment{photo, video, doc}, problem: request.ErrTooManyMedia},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			_, _ = f.svc.Begin(ctx, userID)
			_, _ = f.svc.ChooseType(ctx, userID, requesttype.Complaint)

			s, handled, err := f.svc.HandleMessage(ctx, userID, tt.text, tt.atts)
			require.NoError(t, err)
			assert.True(t, handled)
			if tt.problem == nil {
				assert.Equal(t, StepAddMedia, s.Kind)
				assert.NoError(t, s.Problem)
				return
			}
			assert.Equal(t, StepEnterText, s.Kind, "шаг показывается повторно")
			require.ErrorIs(t, s.Problem, tt.problem)
			assert.Empty(t, s.Draft.Attachments)
		})
	}
}

func TestCreation_MediaLimit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, _ = f.svc.Begin(ctx, userID)
	_, _ = f.svc.ChooseType(ctx, userID, requesttype.Complaint)
	_, _, _ = f.svc.HandleMessage(ctx, userID, "текст", []request.Attachment{photo})

	s, _, err := f.svc.HandleMessage(ctx, userID, "", []request.Attachment{video})
	require.NoError(t, err)
	assert.NoError(t, s.Problem, "ровно на лимите")
	assert.Len(t, s.Draft.Attachments, 2)

	s, _, err = f.svc.HandleMessage(ctx, userID, "", []request.Attachment{doc})
	require.NoError(t, err)
	require.ErrorIs(t, s.Problem, request.ErrTooManyMedia)
	assert.Len(t, s.Draft.Attachments, 2)

	s, _, err = f.svc.HandleMessage(ctx, userID, "просто текст", nil)
	require.NoError(t, err)
	assert.Equal(t, StepAddMedia, s.Kind)
	assert.ErrorIs(t, s.Problem, ErrUnexpectedInput)
}

func TestCreation_UnknownType(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, _ = f.svc.Begin(ctx, userID)

	s, err := f.svc.ChooseType(ctx, userID, "unknown")
	require.NoError(t, err)
	assert.Equal(t, StepChooseType, s.Kind)
	assert.ErrorIs(t, s.Problem, ErrUnexpectedInput)
}

func TestCreation_TextWhileButtonsExpected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, _ = f.svc.Begin(ctx, userID)

	s, handled, err := f.svc.HandleMessage(ctx, userID, "жалоба", nil)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, StepChooseType, s.Kind)
	assert.ErrorIs(t, s.Problem, ErrUnexpectedInput)
}

func TestCreation_StaleButton(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, _ = f.svc.Begin(ctx, userID)
	_, _ = f.svc.ChooseType(ctx, userID, requesttype.Complaint)

	// Кнопка «Опубликовать» со старого сообщения.
	s, err := f.svc.Publish(ctx, author)
	require.NoError(t, err)
	assert.Equal(t, StepEnterText, s.Kind)
	require.ErrorIs(t, s.Problem, ErrUnexpectedInput)
	assert.Empty(t, f.repo.created)
}

func TestCreation_MessageWithoutDraftNotHandled(t *testing.T) {
	f := newFixture(t)

	_, handled, err := f.svc.HandleMessage(context.Background(), userID, "привет", nil)
	require.NoError(t, err)
	assert.False(t, handled)
}

// --- Отмена на каждом шаге ---

func TestCreation_CancelOnEveryStep(t *testing.T) {
	ctx := context.Background()
	steps := map[string]func(f *fixture){
		"выбор типа": func(f *fixture) { _, _ = f.svc.Begin(ctx, userID) },
		"ввод текста": func(f *fixture) {
			_, _ = f.svc.Begin(ctx, userID)
			_, _ = f.svc.ChooseType(ctx, userID, requesttype.Complaint)
		},
		"вложения": func(f *fixture) {
			_, _ = f.svc.Begin(ctx, userID)
			_, _ = f.svc.ChooseType(ctx, userID, requesttype.Complaint)
			_, _, _ = f.svc.HandleMessage(ctx, userID, "текст", nil)
		},
		"анонимность": func(f *fixture) {
			_, _ = f.svc.Begin(ctx, userID)
			_, _ = f.svc.ChooseType(ctx, userID, requesttype.Complaint)
			_, _, _ = f.svc.HandleMessage(ctx, userID, "текст", nil)
			_, _ = f.svc.FinishMedia(ctx, userID)
		},
		"подтверждение": func(f *fixture) { f.toConfirm(t, requesttype.Complaint, false) },
	}
	for name, reach := range steps {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			reach(f)
			_, found, _ := f.store.Get(ctx, "fsm:7")
			require.True(t, found)

			s, err := f.svc.Cancel(ctx, userID)
			require.NoError(t, err)
			assert.Equal(t, StepCancelled, s.Kind)
			_, found, _ = f.store.Get(ctx, "fsm:7")
			assert.False(t, found)

			s, err = f.svc.Publish(ctx, author)
			require.NoError(t, err)
			assert.Equal(t, StepExpired, s.Kind)
			assert.Empty(t, f.repo.created)
		})
	}
}

// --- TTL черновика ---

func TestCreation_DraftTTL(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, _ = f.svc.Begin(ctx, userID)

	// Каждый шаг продлевает TTL.
	f.clock.Advance(fsmTTL - time.Minute)
	s, err := f.svc.ChooseType(ctx, userID, requesttype.Complaint)
	require.NoError(t, err)
	assert.Equal(t, StepEnterText, s.Kind)

	f.clock.Advance(fsmTTL - time.Minute)
	s, handled, err := f.svc.HandleMessage(ctx, userID, "текст", nil)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, StepAddMedia, s.Kind)

	// TTL истёк: следующее действие сообщает об устаревшем черновике.
	f.clock.Advance(fsmTTL)
	s, err = f.svc.FinishMedia(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, StepExpired, s.Kind)
}

func TestCreation_ExpiredOnEveryButton(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	actions := map[string]func() (Step, error){
		"тип":          func() (Step, error) { return f.svc.ChooseType(ctx, userID, requesttype.Complaint) },
		"готово":       func() (Step, error) { return f.svc.FinishMedia(ctx, userID) },
		"анонимность":  func() (Step, error) { return f.svc.ChooseAnonymity(ctx, userID, true) },
		"изменить":     func() (Step, error) { return f.svc.Edit(ctx, userID) },
		"опубликовать": func() (Step, error) { return f.svc.Publish(ctx, author) },
	}
	for name, act := range actions {
		t.Run(name, func(t *testing.T) {
			s, err := act()
			require.NoError(t, err)
			assert.Equal(t, StepExpired, s.Kind)
		})
	}
}

func TestCreation_CorruptedDraftIsExpired(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.store.Set(context.Background(), "fsm:7", "{not json", time.Hour))

	s, err := f.svc.FinishMedia(context.Background(), userID)
	require.NoError(t, err)
	assert.Equal(t, StepExpired, s.Kind)
}

// --- Лимит и порог рейтинга ---

func TestCreation_DailyLimitPerType(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < policy.DailyLimit; i++ {
		f.toConfirm(t, requesttype.Complaint, false)
		s, err := f.svc.Publish(ctx, author)
		require.NoError(t, err)
		require.Equal(t, StepPublished, s.Kind, "жалоба %d в пределах лимита", i+1)
		f.clock.Advance(time.Hour)
	}

	// «Создать заявку» не упирается в лимит: тип ещё не выбран.
	s, err := f.svc.Begin(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, StepChooseType, s.Kind)

	// Жалобы ровно на лимите — выбор типа показывается снова.
	s, err = f.svc.ChooseType(ctx, userID, requesttype.Complaint)
	require.NoError(t, err)
	assert.Equal(t, StepChooseType, s.Kind)
	require.ErrorIs(t, s.Problem, request.ErrDailyLimit)
	assert.Equal(t, policy.DailyLimit, s.Limits.DailyLimit)

	// Другой тип считается отдельно.
	s, err = f.svc.ChooseType(ctx, userID, requesttype.Poll)
	require.NoError(t, err)
	assert.Equal(t, StepEnterText, s.Kind)
	require.NoError(t, s.Problem)

	// Скользящие 24 часа: первая жалоба выходит из окна.
	_, err = f.svc.Cancel(ctx, userID)
	require.NoError(t, err)
	f.clock.Advance(24*time.Hour - 3*time.Hour + time.Second)
	_, err = f.svc.Begin(ctx, userID)
	require.NoError(t, err)
	s, err = f.svc.ChooseType(ctx, userID, requesttype.Complaint)
	require.NoError(t, err)
	assert.Equal(t, StepEnterText, s.Kind)
}

func TestCreation_DailyLimitRecheckedOnPublish(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.toConfirm(t, requesttype.Complaint, false)

	// Пока шёл диалог, лимит жалоб исчерпан (например, в соседней вкладке).
	// Опросы на лимит жалоб не влияют.
	for i := 0; i < policy.DailyLimit; i++ {
		for _, typ := range []requesttype.Code{requesttype.Complaint, requesttype.Poll} {
			_, err := f.repo.Create(ctx, request.Request{ChatID: chatID, AuthorID: userID, Type: typ, CreatedAt: f.clock.now}, nil)
			require.NoError(t, err)
		}
	}

	s, err := f.svc.Publish(ctx, author)
	require.NoError(t, err)
	assert.Equal(t, StepLimitReached, s.Kind)
	assert.Equal(t, requesttype.Complaint, s.Draft.Type, "тип нужен для текста отказа")
	assert.Len(t, f.repo.created, 2*policy.DailyLimit, "новая заявка не создана")
	assert.Empty(t, f.pub.cards)
	assert.Positive(t, f.members.locked, "строка участия блокируется")
}

func TestCreation_ChooseTypeRatingDroppedMeanwhile(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := f.svc.Begin(ctx, userID)
	require.NoError(t, err)
	f.members.m.Rating = -10

	s, err := f.svc.ChooseType(ctx, userID, requesttype.Complaint)
	require.NoError(t, err)
	assert.Equal(t, StepRatingTooLow, s.Kind)
	_, found, _ := f.store.Get(ctx, "fsm:7")
	assert.False(t, found, "черновик сброшен")
}

func TestCreation_RatingThreshold(t *testing.T) {
	tests := []struct {
		name   string
		rating int
		want   StepKind
	}{
		{name: "ровно на пороге", rating: -3, want: StepChooseType},
		{name: "ниже порога", rating: -4, want: StepRatingTooLow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.members.m.Rating = tt.rating

			s, err := f.svc.Begin(context.Background(), userID)
			require.NoError(t, err)
			assert.Equal(t, tt.want, s.Kind)
		})
	}
}

func TestCreation_RatingRecheckedOnPublish(t *testing.T) {
	f := newFixture(t)
	f.toConfirm(t, requesttype.Complaint, false)
	f.members.m.Rating = -10

	s, err := f.svc.Publish(context.Background(), author)
	require.NoError(t, err)
	assert.Equal(t, StepRatingTooLow, s.Kind)
	assert.Empty(t, f.repo.created)
}

// --- Доступ к чату ---

func TestCreation_NoActiveChat(t *testing.T) {
	f := newFixture(t)
	f.svc.Chats = stubActiveChats{}

	s, err := f.svc.Begin(context.Background(), userID)
	require.NoError(t, err)
	assert.Equal(t, StepNoChat, s.Kind)
}

func TestCreation_LeftChatBeforePublish(t *testing.T) {
	f := newFixture(t)
	f.toConfirm(t, requesttype.Complaint, false)
	f.members.err = usecase.ErrNotFound // вышел из чата или чат стал pending

	s, err := f.svc.Publish(context.Background(), author)
	require.NoError(t, err)
	assert.Equal(t, StepNoChat, s.Kind)
	assert.Empty(t, f.repo.created)
}

// --- Сбои внешних систем ---

func TestCreation_PublishFailsAfterCommit(t *testing.T) {
	f := newFixture(t)
	f.toConfirm(t, requesttype.Complaint, false)
	f.pub.err = errors.New("max недоступен")

	s, err := f.svc.Publish(context.Background(), author)
	require.NoError(t, err)
	assert.Equal(t, StepPublished, s.Kind)
	assert.False(t, s.ChatSynced)
	require.Len(t, f.repo.created, 1, "данные в БД не откатываются")
	assert.True(t, f.repo.failed[1], "chat_sync = failed")
}

func TestCreation_DBErrorKeepsDraft(t *testing.T) {
	f := newFixture(t)
	f.toConfirm(t, requesttype.Complaint, false)
	f.repo.createErr = errors.New("pg down")

	_, err := f.svc.Publish(context.Background(), author)
	require.Error(t, err)
	assert.Empty(t, f.pub.cards)
	_, found, _ := f.store.Get(context.Background(), "fsm:7")
	assert.True(t, found, "черновик остаётся, можно повторить")
}

func TestCreation_RedisUnavailable(t *testing.T) {
	f := newFixture(t)
	f.store.err = errors.New("redis down")

	_, err := f.svc.Begin(context.Background(), userID)
	require.Error(t, err, "без Redis диалог не начать")

	_, handled, err := f.svc.HandleMessage(context.Background(), userID, "текст", nil)
	require.Error(t, err)
	assert.False(t, handled)
}
