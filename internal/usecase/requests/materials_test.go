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

	"maxhouse/internal/domain/broadcast"
	"maxhouse/internal/domain/comment"
	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/usecase"
	"maxhouse/internal/usecase/notify"
)

const neighborID = int64(9)

var neighbor = membership.User{ID: neighborID, Nickname: "сосед9"}

// stubMembership — участники: активен, если есть в active.
type stubMembership struct{ active map[int64]bool }

func (s stubMembership) GetActiveForUpdate(_ context.Context, _, userID int64) (membership.Membership, error) {
	if !s.active[userID] {
		return membership.Membership{}, usecase.ErrNotFound
	}
	return membership.Membership{UserID: userID, Status: membership.StatusActive}, nil
}

type fakeComments struct {
	items []comment.Comment
	err   error
}

func (f *fakeComments) Add(_ context.Context, c comment.Comment) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	c.ID = int64(len(f.items) + 1)
	f.items = append(f.items, c)
	return c.ID, nil
}

func (f *fakeComments) List(_ context.Context, requestID int64, offset, limit int) ([]comment.Comment, error) {
	var all []comment.Comment
	for _, c := range f.items {
		if c.RequestID == requestID {
			all = append(all, c)
		}
	}
	if offset >= len(all) {
		return nil, nil
	}
	end := min(offset+limit, len(all))
	return all[offset:end], nil
}

func (f *fakeComments) Count(ctx context.Context, requestID int64) (int, error) {
	all, _ := f.List(ctx, requestID, 0, 1<<30)
	return len(all), nil
}

type stubBrowser struct{ list []request.Request }

func (s stubBrowser) ListActiveInChat(context.Context, int64, int) ([]request.Request, error) {
	return s.list, nil
}

type mockMaterialNotifier struct{ notified []comment.Comment }

func (m *mockMaterialNotifier) MaterialAdded(_ context.Context, _ request.Request, c comment.Comment) bool {
	m.notified = append(m.notified, c)
	return true
}

type materialFixture struct {
	clock    *fakeClock
	store    *fakeFSMStore
	repo     *fakeLifecycle
	comments *fakeComments
	sync     *mockSyncer
	notifier *mockMaterialNotifier
	members  stubMembership
	svc      *Materials
}

func newMaterialFixture(reqs ...request.Request) *materialFixture {
	clock := &fakeClock{now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
	f := &materialFixture{
		clock:    clock,
		store:    newFakeFSMStore(clock),
		repo:     newFakeLifecycle(reqs...),
		comments: &fakeComments{},
		sync:     &mockSyncer{},
		notifier: &mockMaterialNotifier{},
		members:  stubMembership{active: map[int64]bool{userID: true, neighborID: true}},
	}
	f.svc = NewMaterials(MaterialDeps{
		Chats:    stubActiveChats{chat: housechat.Chat{ID: chatID, Title: "Дом"}, ok: true},
		Browser:  stubBrowser{list: reqs},
		Cards:    f.repo,
		Members:  f.members,
		Locker:   f.repo,
		Comments: f.comments,
		Sync:     f.sync,
		Notify:   f.notifier,
		Store:    f.store,
		Tx:       fakeTx{},
		Clock:    clock,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, MaterialConfig{Policy: policy, FSMTTL: fsmTTL, PageSize: 2})
	return f
}

// --- Просмотр ---

func TestView(t *testing.T) {
	f := newMaterialFixture(ownRequest(request.StatusOpen))
	ctx := context.Background()

	res, err := f.svc.View(ctx, reqID, userID)
	require.NoError(t, err)
	assert.Equal(t, MatAuthorCard, res.Kind, "автор видит карточку автора")

	res, err = f.svc.View(ctx, reqID, neighborID)
	require.NoError(t, err)
	assert.Equal(t, MatNeighborCard, res.Kind)
	assert.True(t, res.CanContribute)

	res, err = f.svc.View(ctx, reqID, 404)
	require.NoError(t, err)
	assert.Equal(t, MatNotFound, res.Kind, "не житель этого дома заявку не видит")

	res, err = f.svc.View(ctx, 999, neighborID)
	require.NoError(t, err)
	assert.Equal(t, MatNotFound, res.Kind)
}

func TestView_ClosedAndDeleted(t *testing.T) {
	f := newMaterialFixture(ownRequest(request.StatusClosed))
	res, err := f.svc.View(context.Background(), reqID, neighborID)
	require.NoError(t, err)
	assert.Equal(t, MatNeighborCard, res.Kind)
	assert.False(t, res.CanContribute, "к закрытой заявке материалы не добавляют")

	f = newMaterialFixture(ownRequest(request.StatusDeleted))
	res, err = f.svc.View(context.Background(), reqID, neighborID)
	require.NoError(t, err)
	assert.Equal(t, MatNotFound, res.Kind)
}

func TestHouseRequests(t *testing.T) {
	f := newMaterialFixture(ownRequest(request.StatusOpen))
	res, err := f.svc.HouseRequests(context.Background(), neighborID)
	require.NoError(t, err)
	assert.Equal(t, MatHouseList, res.Kind)
	assert.Len(t, res.Requests, 1)

	f.svc.Chats = stubActiveChats{}
	res, err = f.svc.HouseRequests(context.Background(), neighborID)
	require.NoError(t, err)
	assert.Equal(t, MatNoChat, res.Kind)
}

// --- Комментарий и доказательство ---

func TestComment_HappyPath(t *testing.T) {
	f := newMaterialFixture(ownRequest(request.StatusInProgress))
	ctx := context.Background()

	res, err := f.svc.StartComment(ctx, reqID, neighborID)
	require.NoError(t, err)
	assert.Equal(t, MatAskComment, res.Kind)

	res, handled, err := f.svc.HandleMessage(ctx, neighbor, " Подтверждаю, видел ", nil)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, MatAdded, res.Kind)

	require.Len(t, f.comments.items, 1)
	c := f.comments.items[0]
	assert.Equal(t, comment.KindComment, c.Kind)
	assert.Equal(t, "Подтверждаю, видел", c.Body)
	assert.Equal(t, neighborID, c.AuthorID)
	assert.Equal(t, f.clock.now, c.CreatedAt)
	assert.Equal(t, []int64{reqID}, f.sync.synced, "счётчик материалов в чате обновлён")
	require.Len(t, f.notifier.notified, 1, "автору — уведомление")
	assert.Equal(t, "сосед9", f.notifier.notified[0].AuthorNickname, "автор видит ник отправителя")

	_, found, _ := f.store.Get(ctx, "fsm:9")
	assert.False(t, found)
}

func TestEvidence(t *testing.T) {
	f := newMaterialFixture(ownRequest(request.StatusOpen))
	ctx := context.Background()
	_, err := f.svc.StartEvidence(ctx, reqID, neighborID)
	require.NoError(t, err)

	res, _, err := f.svc.HandleMessage(ctx, neighbor, "только текст", nil)
	require.NoError(t, err)
	assert.Equal(t, MatAskEvidence, res.Kind)
	require.ErrorIs(t, res.Problem, comment.ErrNoMedia)
	assert.Empty(t, f.comments.items)

	res, _, err = f.svc.HandleMessage(ctx, neighbor, "", []request.Attachment{photo})
	require.NoError(t, err)
	assert.Equal(t, MatAdded, res.Kind)
	require.Len(t, f.comments.items, 1)
	assert.Equal(t, comment.KindEvidence, f.comments.items[0].Kind)
	assert.Equal(t, []request.Attachment{photo}, f.comments.items[0].Attachments)
}

func TestStartMaterial_Rejected(t *testing.T) {
	tests := []struct {
		name   string
		status request.Status
		user   int64
		want   MaterialKind
	}{
		{"автор", request.StatusOpen, userID, MatOwnRequest},
		{"закрыта", request.StatusClosed, neighborID, MatNotActive},
		{"истекла", request.StatusExpired, neighborID, MatNotActive},
		{"не житель", request.StatusOpen, 404, MatNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newMaterialFixture(ownRequest(tt.status))
			for _, start := range []func(context.Context, int64, int64) (MaterialResult, error){f.svc.StartComment, f.svc.StartEvidence} {
				res, err := start(context.Background(), reqID, tt.user)
				require.NoError(t, err)
				assert.Equal(t, tt.want, res.Kind)
			}
			assert.Empty(t, f.store.data, "диалог не начинается")
		})
	}
}

func TestMaterial_ClosedWhileWriting(t *testing.T) {
	f := newMaterialFixture(ownRequest(request.StatusOpen))
	ctx := context.Background()
	_, _ = f.svc.StartComment(ctx, reqID, neighborID)
	r := f.repo.reqs[reqID]
	r.Status = request.StatusClosed
	f.repo.reqs[reqID] = r

	res, handled, err := f.svc.HandleMessage(ctx, neighbor, "текст", nil)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, MatNotActive, res.Kind)
	assert.Empty(t, f.comments.items)
	assert.Empty(t, f.notifier.notified)
}

func TestMaterial_DBError(t *testing.T) {
	f := newMaterialFixture(ownRequest(request.StatusOpen))
	f.comments.err = errors.New("pg down")
	ctx := context.Background()
	_, _ = f.svc.StartComment(ctx, reqID, neighborID)

	_, handled, err := f.svc.HandleMessage(ctx, neighbor, "текст", nil)
	require.Error(t, err)
	assert.True(t, handled)
	assert.Empty(t, f.sync.synced)
}

func TestMaterial_CancelAndIsolation(t *testing.T) {
	f := newMaterialFixture(ownRequest(request.StatusOpen))
	ctx := context.Background()
	_, _ = f.svc.StartComment(ctx, reqID, neighborID)

	res, err := f.svc.Cancel(ctx, neighborID)
	require.NoError(t, err)
	assert.Equal(t, MatCancelled, res.Kind)
	_, handled, err := f.svc.HandleMessage(ctx, neighbor, "текст", nil)
	require.NoError(t, err)
	assert.False(t, handled)

	require.NoError(t, saveDraft(ctx, f.store, fsmTTL, neighborID, Draft{State: StateEnteringText}))
	_, handled, err = f.svc.HandleMessage(ctx, neighbor, "текст", nil)
	require.NoError(t, err)
	assert.False(t, handled, "черновик создания не перехватывается")
}

// --- Список материалов для автора ---

func TestListMaterials_Paging(t *testing.T) {
	f := newMaterialFixture(ownRequest(request.StatusOpen))
	for i := 0; i < 3; i++ {
		f.comments.items = append(f.comments.items, comment.Comment{RequestID: reqID, Body: "c"})
	}
	ctx := context.Background()

	res, err := f.svc.ListMaterials(ctx, reqID, userID, 0)
	require.NoError(t, err)
	assert.Equal(t, MatMaterials, res.Kind)
	assert.Len(t, res.Comments, 2)
	assert.Equal(t, 3, res.Total)
	assert.Equal(t, 2, res.NextOffset())

	res, err = f.svc.ListMaterials(ctx, reqID, userID, 2)
	require.NoError(t, err)
	assert.Len(t, res.Comments, 1)
	assert.Zero(t, res.NextOffset(), "последняя страница")

	res, err = f.svc.ListMaterials(ctx, reqID, neighborID, 0)
	require.NoError(t, err)
	assert.Equal(t, MatNotFound, res.Kind, "материалы видит только автор")
}

// --- Рассылка ---

type fakeBroadcasts struct {
	last       time.Time
	has        bool
	recorded   []broadcast.Audience
	recipients map[broadcast.Audience][]int64
}

func (f *fakeBroadcasts) LastAt(context.Context, int64) (time.Time, bool, error) {
	return f.last, f.has, nil
}

func (f *fakeBroadcasts) Record(_ context.Context, _ int64, aud broadcast.Audience, _ int, at time.Time) error {
	f.recorded = append(f.recorded, aud)
	f.last, f.has = at, true
	return nil
}

func (f *fakeBroadcasts) Recipients(_ context.Context, _ int64, aud broadcast.Audience) ([]int64, error) {
	return f.recipients[aud], nil
}

type mockOut struct{ sent map[int64]string }

func (m *mockOut) SendBroadcast(_ context.Context, uid int64, _ request.Request, text string) error {
	m.sent[uid] = text
	return nil
}
func (m *mockOut) NotifyMaterial(context.Context, int64, request.Request, comment.Comment) error {
	return nil
}
func (m *mockOut) ForwardRequest(context.Context, int64, usecase.RequestCard) error { return nil }

// directDeliverer вызывает send для каждого, без пометок недоступности.
type directDeliverer struct{}

func (directDeliverer) Deliver(ctx context.Context, ids []int64, send func(context.Context, int64) error) notify.Delivery {
	var d notify.Delivery
	for _, id := range ids {
		if send(ctx, id) == nil {
			d.Delivered++
		}
	}
	return d
}

type broadcastFixture struct {
	clock *fakeClock
	repo  *fakeBroadcasts
	out   *mockOut
	svc   *Broadcasts
}

func newBroadcastFixture(req request.Request) *broadcastFixture {
	clock := &fakeClock{now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
	f := &broadcastFixture{
		clock: clock,
		repo: &fakeBroadcasts{recipients: map[broadcast.Audience][]int64{
			broadcast.AudienceAll:     {11, 12, 13},
			broadcast.AudienceConfirm: {11, 12},
			broadcast.AudienceRefute:  {13},
		}},
		out: &mockOut{sent: map[int64]string{}},
	}
	lc := newFakeLifecycle(req)
	f.svc = NewBroadcasts(BroadcastDeps{
		Cards:      lc,
		Locker:     lc,
		Broadcasts: f.repo,
		Out:        f.out,
		Deliver:    directDeliverer{},
		Store:      newFakeFSMStore(clock),
		Tx:         fakeTx{},
		Clock:      clock,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, BroadcastConfig{Policy: policy, FSMTTL: fsmTTL, Cooldown: 24 * time.Hour})
	return f
}

func TestBroadcast_Audiences(t *testing.T) {
	for aud, want := range map[broadcast.Audience][]int64{
		broadcast.AudienceAll:     {11, 12, 13},
		broadcast.AudienceConfirm: {11, 12},
		broadcast.AudienceRefute:  {13},
	} {
		t.Run(string(aud), func(t *testing.T) {
			f := newBroadcastFixture(ownRequest(request.StatusInProgress))
			ctx := context.Background()

			res, err := f.svc.Start(ctx, reqID, userID)
			require.NoError(t, err)
			assert.Equal(t, BcChooseAudience, res.Kind)

			res, err = f.svc.ChooseAudience(ctx, reqID, userID, aud)
			require.NoError(t, err)
			assert.Equal(t, BcAskText, res.Kind)

			res, handled, err := f.svc.HandleMessage(ctx, userID, " Собираемся в 19:00 ")
			require.NoError(t, err)
			assert.True(t, handled)
			assert.Equal(t, BcConfirm, res.Kind)
			assert.Equal(t, "Собираемся в 19:00", res.Text)
			assert.Equal(t, len(want), res.Recipients)

			res, err = f.svc.Send(ctx, userID)
			require.NoError(t, err)
			assert.Equal(t, BcSent, res.Kind)
			assert.Equal(t, len(want), res.Delivery.Delivered)
			var got []int64
			for uid, text := range f.out.sent {
				got = append(got, uid)
				assert.Equal(t, "Собираемся в 19:00", text)
			}
			assert.ElementsMatch(t, want, got)
			assert.Equal(t, []broadcast.Audience{aud}, f.repo.recorded, "факт рассылки записан")
		})
	}
}

func TestBroadcast_Cooldown(t *testing.T) {
	f := newBroadcastFixture(ownRequest(request.StatusOpen))
	ctx := context.Background()
	f.repo.last, f.repo.has = f.clock.now.Add(-23*time.Hour), true

	res, err := f.svc.Start(ctx, reqID, userID)
	require.NoError(t, err)
	assert.Equal(t, BcCooldown, res.Kind)
	assert.Equal(t, f.clock.now.Add(time.Hour), res.NextAt)

	f.clock.now = f.clock.now.Add(time.Hour)
	res, err = f.svc.Start(ctx, reqID, userID)
	require.NoError(t, err)
	assert.Equal(t, BcChooseAudience, res.Kind, "ровно через кулдаун можно")
}

func TestBroadcast_CooldownRecheckedOnSend(t *testing.T) {
	f := newBroadcastFixture(ownRequest(request.StatusOpen))
	ctx := context.Background()
	_, _ = f.svc.ChooseAudience(ctx, reqID, userID, broadcast.AudienceAll)
	_, _, _ = f.svc.HandleMessage(ctx, userID, "текст")

	// Пока автор подтверждал, рассылка ушла из другой вкладки.
	f.repo.last, f.repo.has = f.clock.now, true

	res, err := f.svc.Send(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, BcCooldown, res.Kind)
	assert.Empty(t, f.out.sent)
}

func TestBroadcast_NotAuthor(t *testing.T) {
	f := newBroadcastFixture(ownRequest(request.StatusOpen))
	res, err := f.svc.Start(context.Background(), reqID, neighborID)
	require.NoError(t, err)
	assert.Equal(t, BcNotFound, res.Kind)

	f = newBroadcastFixture(ownRequest(request.StatusDeleted))
	res, err = f.svc.Start(context.Background(), reqID, userID)
	require.NoError(t, err)
	assert.Equal(t, BcNotFound, res.Kind)
}

func TestBroadcast_EmptyTextAndCancel(t *testing.T) {
	f := newBroadcastFixture(ownRequest(request.StatusOpen))
	ctx := context.Background()
	_, _ = f.svc.ChooseAudience(ctx, reqID, userID, broadcast.AudienceAll)

	res, _, err := f.svc.HandleMessage(ctx, userID, "  ")
	require.NoError(t, err)
	assert.Equal(t, BcAskText, res.Kind)
	require.ErrorIs(t, res.Problem, request.ErrEmptyBody)

	res, err = f.svc.Cancel(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, BcCancelled, res.Kind)

	res, err = f.svc.Send(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, BcExpired, res.Kind)
	assert.Empty(t, f.repo.recorded)
}

func TestBroadcast_TextOnConfirmStep(t *testing.T) {
	f := newBroadcastFixture(ownRequest(request.StatusOpen))
	ctx := context.Background()
	_, _ = f.svc.ChooseAudience(ctx, reqID, userID, broadcast.AudienceRefute)
	_, _, _ = f.svc.HandleMessage(ctx, userID, "первый")

	res, _, err := f.svc.HandleMessage(ctx, userID, "второй")
	require.NoError(t, err)
	assert.Equal(t, BcConfirm, res.Kind)
	assert.Equal(t, "первый", res.Text, "текст не подменяется на шаге подтверждения")
	assert.ErrorIs(t, res.Problem, ErrUnexpectedInput)
}

// --- Определение диалога ---

func TestDialogs_Current(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	store := newFakeFSMStore(clock)
	d := NewDialogs(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	flow, err := d.Current(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, FlowNone, flow)

	for state, want := range map[State]Flow{
		StateAddingMedia:      FlowCreate,
		StateEditingBody:      FlowManage,
		StateAddingEvidence:   FlowMaterial,
		StateBroadcastConfirm: FlowBroadcast,
	} {
		require.NoError(t, saveDraft(ctx, store, time.Hour, userID, Draft{State: state}))
		flow, err := d.Current(ctx, userID)
		require.NoError(t, err)
		assert.Equal(t, want, flow, state)
	}

	require.NoError(t, d.Reset(ctx, userID))
	flow, err = d.Current(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, FlowNone, flow, "после сброса диалога нет")
	require.NoError(t, d.Reset(ctx, userID), "сброс без диалога — не ошибка")

	store.err = errors.New("redis down")
	_, err = d.Current(ctx, userID)
	require.Error(t, err)
	require.Error(t, d.Reset(ctx, userID))
}
