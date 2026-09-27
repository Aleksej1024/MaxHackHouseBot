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
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/usecase"
)

// fakeLifecycle — in-memory заявки: RequestLocker, RequestLifecycleRepo, CardReader.
type fakeLifecycle struct {
	reqs    map[int64]request.Request
	saveErr error
}

func newFakeLifecycle(reqs ...request.Request) *fakeLifecycle {
	f := &fakeLifecycle{reqs: map[int64]request.Request{}}
	for _, r := range reqs {
		f.reqs[r.ID] = r
	}
	return f
}

func (f *fakeLifecycle) GetForUpdate(_ context.Context, id int64) (request.Request, error) {
	r, ok := f.reqs[id]
	if !ok {
		return request.Request{}, usecase.ErrNotFound
	}
	return r, nil
}

func (f *fakeLifecycle) ClaimRating(context.Context, int64) (bool, error) { return true, nil }

func (f *fakeLifecycle) LoadCard(_ context.Context, id int64) (usecase.RequestCard, error) {
	r, ok := f.reqs[id]
	if !ok {
		return usecase.RequestCard{}, usecase.ErrNotFound
	}
	return usecase.RequestCard{Request: r}, nil
}

func (f *fakeLifecycle) NextVotingEnded(context.Context, time.Time) (request.Request, bool, error) {
	return request.Request{}, false, nil
}

func (f *fakeLifecycle) NextExpired(context.Context, time.Time) (request.Request, bool, error) {
	return request.Request{}, false, nil
}

func (f *fakeLifecycle) SaveState(_ context.Context, r request.Request) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	cur := f.reqs[r.ID]
	cur.Status, cur.ResultText, cur.ClosedAt = r.Status, r.ResultText, r.ClosedAt
	f.reqs[r.ID] = cur
	return nil
}

func (f *fakeLifecycle) UpdateBody(_ context.Context, id int64, body string) error {
	cur := f.reqs[id]
	cur.Body = body
	f.reqs[id] = cur
	return nil
}

func (f *fakeLifecycle) ListByAuthor(_ context.Context, chatID, authorID int64, limit int) ([]request.Request, error) {
	var out []request.Request
	for id := int64(1); id <= int64(len(f.reqs)) && len(out) < limit; id++ {
		r := f.reqs[id]
		if r.ChatID == chatID && r.AuthorID == authorID && r.Status != request.StatusDeleted {
			out = append(out, r)
		}
	}
	return out, nil
}

// mockRater запоминает, по каким заявкам считался рейтинг.
type mockRater struct{ applied []request.Request }

func (m *mockRater) Apply(_ context.Context, r request.Request) (int, error) {
	m.applied = append(m.applied, r)
	return 0, nil
}

// mockSyncer запоминает синхронизации и публикации итогов.
type mockSyncer struct {
	synced, results []int64
	fail            bool
}

func (m *mockSyncer) Sync(_ context.Context, id int64) bool {
	m.synced = append(m.synced, id)
	return !m.fail
}

func (m *mockSyncer) PublishResult(_ context.Context, id int64) bool {
	m.results = append(m.results, id)
	return !m.fail
}

const (
	otherUser = int64(8)
	reqID     = int64(1)
)

type manageFixture struct {
	clock *fakeClock
	store *fakeFSMStore
	repo  *fakeLifecycle
	rater *mockRater
	sync  *mockSyncer
	svc   *Management
}

func newManageFixture(reqs ...request.Request) *manageFixture {
	clock := &fakeClock{now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	f := &manageFixture{
		clock: clock,
		store: newFakeFSMStore(clock),
		repo:  newFakeLifecycle(reqs...),
		rater: &mockRater{},
		sync:  &mockSyncer{},
	}
	f.svc = NewManagement(ManageDeps{
		Chats:     stubActiveChats{chat: housechat.Chat{ID: chatID, Title: "Дом"}, ok: true},
		Locker:    f.repo,
		Lifecycle: f.repo,
		Cards:     f.repo,
		Rater:     f.rater,
		Sync:      f.sync,
		Store:     f.store,
		Tx:        fakeTx{},
		Clock:     clock,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, ManageConfig{Policy: policy, FSMTTL: fsmTTL, ListLimit: 2})
	return f
}

func ownRequest(status request.Status) request.Request {
	return request.Request{ID: reqID, ChatID: chatID, AuthorID: userID, Type: requesttype.Complaint, Status: status, Body: "старый"}
}

// --- Список и карточка ---

func TestMyRequests(t *testing.T) {
	deleted := ownRequest(request.StatusDeleted)
	deleted.ID = 2
	foreign := ownRequest(request.StatusOpen)
	foreign.ID, foreign.AuthorID = 3, otherUser
	closed := ownRequest(request.StatusClosed)
	closed.ID = 4
	third := ownRequest(request.StatusOpen)
	third.ID = 5
	f := newManageFixture(ownRequest(request.StatusOpen), deleted, foreign, closed, third)

	res, err := f.svc.MyRequests(context.Background(), userID)
	require.NoError(t, err)
	assert.Equal(t, ManageList, res.Kind)
	require.Len(t, res.Requests, 2, "лимит списка из конфига")
	assert.Equal(t, []int64{1, 4}, []int64{res.Requests[0].ID, res.Requests[1].ID}, "без удалённых и чужих")
}

func TestMyRequests_NoChat(t *testing.T) {
	f := newManageFixture()
	f.svc.Chats = stubActiveChats{}

	res, err := f.svc.MyRequests(context.Background(), userID)
	require.NoError(t, err)
	assert.Equal(t, ManageNoChat, res.Kind)
}

func TestOpen(t *testing.T) {
	f := newManageFixture(ownRequest(request.StatusOpen))

	res, err := f.svc.Open(context.Background(), reqID, userID)
	require.NoError(t, err)
	assert.Equal(t, ManageCard, res.Kind)

	res, err = f.svc.Open(context.Background(), reqID, otherUser)
	require.NoError(t, err)
	assert.Equal(t, ManageNotFound, res.Kind, "чужую заявку автор-карточкой не показываем")

	res, err = f.svc.Open(context.Background(), 404, userID)
	require.NoError(t, err)
	assert.Equal(t, ManageNotFound, res.Kind)
}

// --- Закрытие ---

func TestClose_FromOpenAppliesRating(t *testing.T) {
	f := newManageFixture(ownRequest(request.StatusOpen))
	ctx := context.Background()

	res, err := f.svc.StartClose(ctx, reqID, userID)
	require.NoError(t, err)
	assert.Equal(t, ManageAskResult, res.Kind)

	res, handled, err := f.svc.HandleMessage(ctx, userID, "  Сосед извинился  ")
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, ManageClosed, res.Kind)
	assert.True(t, res.ChatSynced)

	got := f.repo.reqs[reqID]
	assert.Equal(t, request.StatusClosed, got.Status)
	assert.Equal(t, "Сосед извинился", got.ResultText)
	assert.Equal(t, f.clock.now, got.ClosedAt)
	require.Len(t, f.rater.applied, 1, "досрочное завершение голосования — рейтинг")
	assert.Equal(t, request.StatusClosed, f.rater.applied[0].Status)
	assert.Equal(t, []int64{reqID}, f.sync.synced, "сообщение в чате обновлено")
	assert.Equal(t, []int64{reqID}, f.sync.results, "итог опубликован")

	_, found, _ := f.store.Get(ctx, "fsm:7")
	assert.False(t, found)
}

func TestClose_FromInProgress(t *testing.T) {
	f := newManageFixture(ownRequest(request.StatusInProgress))
	ctx := context.Background()
	_, _ = f.svc.StartClose(ctx, reqID, userID)

	res, _, err := f.svc.HandleMessage(ctx, userID, "Готово")
	require.NoError(t, err)
	assert.Equal(t, ManageClosed, res.Kind)
	assert.Equal(t, request.StatusClosed, f.repo.reqs[reqID].Status)
}

func TestClose_EmptyResultAsksAgain(t *testing.T) {
	f := newManageFixture(ownRequest(request.StatusOpen))
	ctx := context.Background()
	_, _ = f.svc.StartClose(ctx, reqID, userID)

	res, handled, err := f.svc.HandleMessage(ctx, userID, "   ")
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, ManageAskResult, res.Kind)
	require.ErrorIs(t, res.Problem, request.ErrEmptyBody)
	assert.Equal(t, request.StatusOpen, f.repo.reqs[reqID].Status)

	res, _, err = f.svc.HandleMessage(ctx, userID, "итог")
	require.NoError(t, err)
	assert.Equal(t, ManageClosed, res.Kind, "диалог продолжается после ошибки ввода")
}

func TestClose_ClosedMeanwhile(t *testing.T) {
	f := newManageFixture(ownRequest(request.StatusOpen))
	ctx := context.Background()
	_, _ = f.svc.StartClose(ctx, reqID, userID)

	// Пока автор писал итог, планировщик перевёл заявку в expired.
	r := f.repo.reqs[reqID]
	r.Status = request.StatusExpired
	f.repo.reqs[reqID] = r

	res, _, err := f.svc.HandleMessage(ctx, userID, "итог")
	require.NoError(t, err)
	assert.Equal(t, ManageNotActive, res.Kind)
	assert.Empty(t, f.rater.applied)
	assert.Empty(t, f.sync.synced)
}

func TestStartActions_Rejected(t *testing.T) {
	tests := []struct {
		name   string
		status request.Status
		user   int64
		want   ManageKind
	}{
		{name: "чужая заявка", status: request.StatusOpen, user: otherUser, want: ManageNotFound},
		{name: "закрыта", status: request.StatusClosed, user: userID, want: ManageNotActive},
		{name: "истекла", status: request.StatusExpired, user: userID, want: ManageNotActive},
		{name: "удалена", status: request.StatusDeleted, user: userID, want: ManageNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newManageFixture(ownRequest(tt.status))
			ctx := context.Background()
			for _, act := range []func(context.Context, int64, int64) (ManageResult, error){
				f.svc.StartClose, f.svc.StartEdit, f.svc.AskDelete, f.svc.Delete,
			} {
				res, err := act(ctx, reqID, tt.user)
				require.NoError(t, err)
				assert.Equal(t, tt.want, res.Kind)
			}
			_, found, _ := f.store.Get(ctx, "fsm:"+itoa(tt.user))
			assert.False(t, found, "диалог не начинается")
			assert.Equal(t, tt.status, f.repo.reqs[reqID].Status)
		})
	}
}

func itoa(n int64) string {
	return map[int64]string{userID: "7", otherUser: "8"}[n]
}

// --- Правка ---

func TestEdit_KeepsStatusAndSyncs(t *testing.T) {
	f := newManageFixture(ownRequest(request.StatusInProgress))
	ctx := context.Background()

	res, err := f.svc.StartEdit(ctx, reqID, userID)
	require.NoError(t, err)
	assert.Equal(t, ManageAskBody, res.Kind)

	res, handled, err := f.svc.HandleMessage(ctx, userID, "новый текст")
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, ManageEdited, res.Kind)
	assert.Equal(t, "новый текст", f.repo.reqs[reqID].Body)
	assert.Equal(t, request.StatusInProgress, f.repo.reqs[reqID].Status)
	assert.Empty(t, f.rater.applied, "правка не завершает голосование")
	assert.Equal(t, []int64{reqID}, f.sync.synced)
	assert.Empty(t, f.sync.results)
}

func TestEdit_TooLong(t *testing.T) {
	f := newManageFixture(ownRequest(request.StatusOpen))
	ctx := context.Background()
	_, _ = f.svc.StartEdit(ctx, reqID, userID)

	res, _, err := f.svc.HandleMessage(ctx, userID, "этот текст точно длиннее двадцати символов")
	require.NoError(t, err)
	assert.Equal(t, ManageAskBody, res.Kind)
	require.ErrorIs(t, res.Problem, request.ErrBodyTooLong)
	assert.Equal(t, "старый", f.repo.reqs[reqID].Body)
}

// --- Удаление ---

func TestDelete(t *testing.T) {
	for _, st := range []request.Status{request.StatusOpen, request.StatusInProgress} {
		t.Run(string(st), func(t *testing.T) {
			f := newManageFixture(ownRequest(st))
			ctx := context.Background()

			res, err := f.svc.AskDelete(ctx, reqID, userID)
			require.NoError(t, err)
			assert.Equal(t, ManageConfirmDelete, res.Kind)
			assert.Equal(t, st, f.repo.reqs[reqID].Status, "без подтверждения не удаляем")

			res, err = f.svc.Delete(ctx, reqID, userID)
			require.NoError(t, err)
			assert.Equal(t, ManageDeleted, res.Kind)
			assert.Equal(t, request.StatusDeleted, f.repo.reqs[reqID].Status)
			assert.Empty(t, f.rater.applied, "рейтинг по удалённой заявке не считается")
			assert.Equal(t, []int64{reqID}, f.sync.synced, "сообщение в чате удаляется")

			res, err = f.svc.Delete(ctx, reqID, userID)
			require.NoError(t, err)
			assert.Equal(t, ManageNotFound, res.Kind, "повторное удаление безопасно")
		})
	}
}

func TestDelete_ChatSyncFailedReported(t *testing.T) {
	f := newManageFixture(ownRequest(request.StatusOpen))
	f.sync.fail = true

	res, err := f.svc.Delete(context.Background(), reqID, userID)
	require.NoError(t, err)
	assert.Equal(t, ManageDeleted, res.Kind, "данные в БД не откатываются")
	assert.False(t, res.ChatSynced)
}

func TestDelete_DBError(t *testing.T) {
	f := newManageFixture(ownRequest(request.StatusOpen))
	f.repo.saveErr = errors.New("pg down")

	_, err := f.svc.Delete(context.Background(), reqID, userID)
	require.Error(t, err)
	assert.Empty(t, f.sync.synced)
}

// --- Диалоги ---

func TestManage_CancelAndDialogIsolation(t *testing.T) {
	f := newManageFixture(ownRequest(request.StatusOpen))
	ctx := context.Background()
	_, _ = f.svc.StartClose(ctx, reqID, userID)

	res, err := f.svc.Cancel(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, ManageCancelled, res.Kind)

	_, handled, err := f.svc.HandleMessage(ctx, userID, "итог")
	require.NoError(t, err)
	assert.False(t, handled, "после отмены текст уходит в меню")
	assert.Equal(t, request.StatusOpen, f.repo.reqs[reqID].Status)
}

func TestManage_IgnoresCreationDialog(t *testing.T) {
	f := newManageFixture(ownRequest(request.StatusOpen))
	ctx := context.Background()
	require.NoError(t, saveDraft(ctx, f.store, fsmTTL, userID, Draft{State: StateEnteringText, ChatID: chatID}))

	_, handled, err := f.svc.HandleMessage(ctx, userID, "текст")
	require.NoError(t, err)
	assert.False(t, handled, "диалог создания не перехватывается")

	_, err = f.svc.Cancel(ctx, userID)
	require.NoError(t, err)
	_, found, _ := f.store.Get(ctx, "fsm:7")
	assert.True(t, found, "отмена управления не трогает черновик создания")
}

func TestCreation_IgnoresManagementDialog(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	require.NoError(t, saveDraft(ctx, f.store, fsmTTL, userID, Draft{State: StateEnteringResult, RequestID: 1}))

	_, handled, err := f.svc.HandleMessage(ctx, userID, "итог", nil)
	require.NoError(t, err)
	assert.False(t, handled)

	s, err := f.svc.FinishMedia(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, StepExpired, s.Kind, "старая кнопка создания не ломает диалог закрытия")
	d, found, _ := loadDraft(ctx, f.store, f.svc.Log, userID)
	assert.True(t, found)
	assert.Equal(t, StateEnteringResult, d.State)
}

func TestManage_StartCreationReplacesDialog(t *testing.T) {
	f := newManageFixture(ownRequest(request.StatusOpen))
	ctx := context.Background()
	_, _ = f.svc.StartEdit(ctx, reqID, userID)

	// Новый диалог (здесь — ещё одно закрытие) перезаписывает прежний.
	_, _ = f.svc.StartClose(ctx, reqID, userID)
	res, _, err := f.svc.HandleMessage(ctx, userID, "итог")
	require.NoError(t, err)
	assert.Equal(t, ManageClosed, res.Kind)
	assert.Equal(t, "старый", f.repo.reqs[reqID].Body)
}
