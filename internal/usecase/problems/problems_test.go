package problems

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/problem"
)

// --- Тестовые дублёры ---

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

type fakeTx struct{}

func (fakeTx) Do(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

type stubChats struct{ ok bool }

func (s stubChats) ActiveChat(context.Context, int64) (housechat.Chat, bool, error) {
	return housechat.Chat{ID: chatID, Title: "Дом"}, s.ok, nil
}

// fakeRepo — in-memory окна и жители с той же семантикой, что SQL:
// житель в окне один раз, Current считает жителей.
type fakeRepo struct {
	incidents []problem.Incident
	reporters []reporter
	locks     int
	err       error // сбой записи жителя или отметки
}

type reporter struct {
	incidentID, userID int64
	at                 time.Time
}

func (r *fakeRepo) Lock(context.Context, int64, problem.Code) error {
	r.locks++
	return nil
}

func (r *fakeRepo) Current(_ context.Context, chatID int64, code problem.Code, openSince time.Time) (problem.Incident, bool, error) {
	for i := len(r.incidents) - 1; i >= 0; i-- {
		inc := r.incidents[i]
		if inc.ChatID == chatID && inc.Problem == code && inc.FirstReportedAt.After(openSince) {
			inc.Reporters = len(r.users(inc.ID))
			return inc, true, nil
		}
	}
	return problem.Incident{}, false, nil
}

func (r *fakeRepo) Create(_ context.Context, i problem.Incident) (int64, error) {
	i.ID = int64(len(r.incidents) + 1)
	r.incidents = append(r.incidents, i)
	return i.ID, nil
}

func (r *fakeRepo) AddReporter(_ context.Context, incidentID, userID int64, at time.Time) (bool, error) {
	if r.err != nil {
		return false, r.err
	}
	if slices.Contains(r.users(incidentID), userID) {
		return false, nil
	}
	r.reporters = append(r.reporters, reporter{incidentID, userID, at})
	return true, nil
}

func (r *fakeRepo) SetNotified(_ context.Context, incidentID int64, at time.Time) error {
	if r.err != nil {
		return r.err
	}
	inc := &r.incidents[incidentID-1]
	inc.Notified, inc.NotifiedAt = !at.IsZero(), at
	return nil
}

// users — жители окна в порядке сообщений.
func (r *fakeRepo) users(incidentID int64) []int64 {
	var ids []int64
	for _, rp := range r.reporters {
		if rp.incidentID == incidentID {
			ids = append(ids, rp.userID)
		}
	}
	return ids
}

type stubMembers struct {
	members []membership.Member
	err     error
}

func (s stubMembers) ListMembers(context.Context, int64) ([]membership.Member, error) {
	return s.members, s.err
}

func (s stubMembers) ChatTitle(context.Context, int64) (string, error) { return "", nil }

type stubAdmins struct{ admins []membership.User }

func (s stubAdmins) ListChatAdmins(context.Context, int64) ([]membership.User, error) {
	return s.admins, nil
}

// mockAlerter запоминает оповещения; err — сбой MAX.
type mockAlerter struct {
	alerts []alert
	err    error
}

type alert struct {
	chatID int64
	code   problem.Code
	admins []membership.User
}

func (m *mockAlerter) SendProblemAlert(_ context.Context, chatID int64, code problem.Code, admins []membership.User) error {
	m.alerts = append(m.alerts, alert{chatID, code, admins})
	return m.err
}

// --- Сборка ---

const chatID = int64(-100)

var t0 = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

type fixture struct {
	clock   *fakeClock
	repo    *fakeRepo
	alerter *mockAlerter
	members *stubMembers
	svc     *Service
}

func newFixture() *fixture {
	f := &fixture{
		clock:   &fakeClock{now: t0},
		repo:    &fakeRepo{},
		alerter: &mockAlerter{},
		members: &stubMembers{members: []membership.Member{
			{UserID: 1, Nickname: "Алиса", IsAdmin: true},
			{UserID: 2, Nickname: "Борис"},
			{UserID: 99, Nickname: "Бот", IsAdmin: true, IsBot: true},
		}},
	}
	f.svc = New(Deps{
		Chats:   stubChats{ok: true},
		Repo:    f.repo,
		Members: f.members,
		Admins:  stubAdmins{admins: []membership.User{{ID: 7, Nickname: "старый админ"}}},
		Alerter: f.alerter,
		Tx:      fakeTx{},
		Clock:   f.clock,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, problem.Policy{Threshold: 3, Window: 30 * time.Minute})
	return f
}

func (f *fixture) report(t *testing.T, userID int64, code problem.Code) Result {
	t.Helper()
	res, err := f.svc.Report(context.Background(), userID, code)
	require.NoError(t, err)
	return res
}

// --- Сценарии ---

func TestList(t *testing.T) {
	f := newFixture()
	res, err := f.svc.List(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, OutcomeList, res.Outcome)
	assert.Equal(t, problem.Codes, res.Problems)

	f.svc.Chats = stubChats{}
	res, err = f.svc.List(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, OutcomeNoChat, res.Outcome)
}

func TestReport_AlertAtThreshold(t *testing.T) {
	f := newFixture()

	assert.Equal(t, Result{Outcome: OutcomeRecorded, Problem: problem.NoWater}, f.report(t, 10, problem.NoWater))
	assert.Equal(t, OutcomeAlreadyReported, f.report(t, 10, problem.NoWater).Outcome, "повторное нажатие не считается")
	f.clock.now = t0.Add(10 * time.Minute)
	res := f.report(t, 11, problem.NoWater)
	assert.False(t, res.Alerted, "двое из трёх")
	assert.Empty(t, f.alerter.alerts)

	f.clock.now = t0.Add(29 * time.Minute)
	res = f.report(t, 12, problem.NoWater)
	assert.True(t, res.Alerted, "третий житель в окне — оповещение")
	require.Len(t, f.alerter.alerts, 1)
	a := f.alerter.alerts[0]
	assert.Equal(t, chatID, a.chatID)
	assert.Equal(t, problem.NoWater, a.code)
	assert.Equal(t, []membership.User{{ID: 1, Nickname: "Алиса"}}, a.admins, "админы из MAX, без ботов")

	require.Len(t, f.repo.incidents, 1, "все сообщения в одном окне")
	inc := f.repo.incidents[0]
	assert.Equal(t, []int64{10, 11, 12}, f.repo.users(1))
	assert.Equal(t, t0.Add(29*time.Minute), f.repo.reporters[2].at, "время сообщения жителя")
	assert.True(t, inc.Notified)
	assert.Equal(t, t0.Add(29*time.Minute), inc.NotifiedAt)
	assert.Equal(t, 4, f.repo.locks, "каждое нажатие, включая повторное, — под блокировкой")
}

func TestReport_OneAlertPerWindow(t *testing.T) {
	f := newFixture()
	for _, uid := range []int64{10, 11, 12, 13, 14} {
		f.report(t, uid, problem.NoPower)
	}
	assert.Len(t, f.alerter.alerts, 1, "после оповещения в окне — только копим жителей")
	assert.Equal(t, []int64{10, 11, 12, 13, 14}, f.repo.users(1))
}

func TestReport_NewWindowAfterExpiry(t *testing.T) {
	f := newFixture()
	f.report(t, 10, problem.NoGas)
	f.report(t, 11, problem.NoGas)

	// Окно (30 минут с первого сообщения) закрылось — счёт заново.
	f.clock.now = t0.Add(30 * time.Minute)
	res := f.report(t, 12, problem.NoGas)
	assert.False(t, res.Alerted, "третий, но уже в новом окне")
	require.Len(t, f.repo.incidents, 2)
	assert.Equal(t, []int64{12}, f.repo.users(2))
	assert.Equal(t, t0.Add(30*time.Minute), f.repo.incidents[1].FirstReportedAt)

	// После оповещения и окна — снова можно оповестить.
	f.report(t, 10, problem.NoGas)
	assert.True(t, f.report(t, 11, problem.NoGas).Alerted)
}

func TestReport_ProblemsAndChatsSeparate(t *testing.T) {
	f := newFixture()
	f.report(t, 10, problem.NoWater)
	f.report(t, 11, problem.NoPower)
	f.report(t, 12, problem.Drone)
	assert.Empty(t, f.alerter.alerts, "разные проблемы считаются отдельно")
	assert.Len(t, f.repo.incidents, 3)
}

func TestReport_SendFailureRetriedByNextReport(t *testing.T) {
	f := newFixture()
	f.alerter.err = errors.New("max down")
	f.report(t, 10, problem.NoInternet)
	f.report(t, 11, problem.NoInternet)
	res := f.report(t, 12, problem.NoInternet)
	assert.False(t, res.Alerted)
	assert.False(t, f.repo.incidents[0].Notified, "отметка снята — оповещение не потеряно")

	f.alerter.err = nil
	res = f.report(t, 13, problem.NoInternet)
	assert.True(t, res.Alerted, "следующее сообщение повторяет попытку")
	assert.Len(t, f.alerter.alerts, 2)
	assert.True(t, f.repo.incidents[0].Notified)
}

func TestReport_AdminsFallbackToDB(t *testing.T) {
	f := newFixture()
	f.members.err = errors.New("max down")
	for _, uid := range []int64{10, 11, 12} {
		f.report(t, uid, problem.Drone)
	}
	require.Len(t, f.alerter.alerts, 1)
	assert.Equal(t, []membership.User{{ID: 7, Nickname: "старый админ"}}, f.alerter.alerts[0].admins)
}

func TestReport_Rejected(t *testing.T) {
	f := newFixture()
	assert.Equal(t, OutcomeUnknown, f.report(t, 10, "meteor").Outcome, "данным кнопки не доверяем")

	f.svc.Chats = stubChats{}
	assert.Equal(t, OutcomeNoChat, f.report(t, 10, problem.NoWater).Outcome)
	assert.Empty(t, f.repo.incidents)
}

func TestReport_DBError(t *testing.T) {
	f := newFixture()
	f.report(t, 10, problem.NoWater)
	f.repo.err = errors.New("pg down")

	_, err := f.svc.Report(context.Background(), 11, problem.NoWater)
	require.Error(t, err)
	assert.Empty(t, f.alerter.alerts)
}
