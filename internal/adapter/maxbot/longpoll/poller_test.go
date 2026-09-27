package longpoll

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pollResult — ответ fakeSource на очередной GetUpdates.
type pollResult struct {
	updates []model.Update
	marker  int64
	err     error
}

// fakeSource отдаёт заготовленные ответы по очереди; когда они кончаются,
// отменяет контекст (как остановка бота).
type fakeSource struct {
	results      []pollResult
	markers      []int64
	subs         []model.Subscription
	subsErr      error
	unsubscribed []string
	cancel       context.CancelFunc
}

func (f *fakeSource) GetUpdates(ctx context.Context, marker int64) ([]model.Update, int64, error) {
	f.markers = append(f.markers, marker)
	if len(f.results) == 0 {
		f.cancel()
		<-ctx.Done()
		return nil, 0, ctx.Err()
	}
	r := f.results[0]
	f.results = f.results[1:]
	return r.updates, r.marker, r.err
}

func (f *fakeSource) GetSubscriptions(context.Context) (model.GetSubscriptionsResult, error) {
	return model.GetSubscriptionsResult{Subscriptions: f.subs}, f.subsErr
}

func (f *fakeSource) Unsubscribe(_ context.Context, url string) (model.SimpleQueryResult, error) {
	f.unsubscribed = append(f.unsubscribed, url)
	return model.SimpleQueryResult{Success: true}, nil
}

type fakeIntake struct {
	got []model.Update
	err error
}

func (f *fakeIntake) AcceptWait(_ context.Context, upd model.Update) error {
	if f.err != nil {
		return f.err
	}
	f.got = append(f.got, upd)
	return nil
}

type fixture struct {
	src    *fakeSource
	intake *fakeIntake
	sleeps []time.Duration
	poller *Poller
	ctx    context.Context
}

func newFixture(results ...pollResult) *fixture {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fixture{src: &fakeSource{results: results, cancel: cancel}, intake: &fakeIntake{}, ctx: ctx}
	f.poller = New(Options{
		Source:    f.src,
		Intake:    f.intake,
		BaseDelay: time.Second,
		MaxDelay:  3 * time.Second,
		Sleep: func(_ context.Context, d time.Duration) error {
			f.sleeps = append(f.sleeps, d)
			return nil
		},
	})
	return f
}

func upd(id string) model.Update {
	return model.Update{UpdateType: model.UpdateMessageCreated, MessageID: id}
}

func TestRun_DeliversUpdatesAndAdvancesMarker(t *testing.T) {
	f := newFixture(
		pollResult{updates: []model.Update{upd("a"), upd("b")}, marker: 10},
		pollResult{marker: 0}, // пустой ответ без marker — marker не сбрасываем
		pollResult{updates: []model.Update{upd("c")}, marker: 11},
	)

	require.NoError(t, f.poller.Run(f.ctx))

	var ids []string
	for _, u := range f.intake.got {
		ids = append(ids, u.MessageID)
	}
	assert.Equal(t, []string{"a", "b", "c"}, ids, "порядок обновлений сохраняется")
	assert.Equal(t, []int64{0, 10, 10, 11}, f.src.markers,
		"первый запрос без marker (неподтверждённые), дальше — marker из ответа")
}

func TestRun_BackoffOnErrors(t *testing.T) {
	boom := errors.New("network")
	f := newFixture(
		pollResult{err: boom},
		pollResult{err: boom},
		pollResult{err: boom},
		pollResult{err: boom},
		pollResult{updates: []model.Update{upd("a")}, marker: 5},
		pollResult{err: boom},
	)

	require.NoError(t, f.poller.Run(f.ctx))

	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 3 * time.Second, time.Second},
		f.sleeps, "пауза растёт до MaxDelay и сбрасывается после успеха")
	assert.Len(t, f.intake.got, 1, "ошибка опроса не останавливает бота")
}

func TestRun_DropsWebhookSubscriptions(t *testing.T) {
	f := newFixture()
	f.src.subs = []model.Subscription{{URL: "https://old.example/webhook"}}

	require.NoError(t, f.poller.Run(f.ctx))
	assert.Equal(t, []string{"https://old.example/webhook"}, f.src.unsubscribed)
}

func TestRun_SubscriptionsErrorNotFatal(t *testing.T) {
	f := newFixture(pollResult{updates: []model.Update{upd("a")}, marker: 1})
	f.src.subsErr = errors.New("403")

	require.NoError(t, f.poller.Run(f.ctx))
	assert.Len(t, f.intake.got, 1)
}

func TestRun_StopsWhenIntakeCancelled(t *testing.T) {
	f := newFixture(
		pollResult{updates: []model.Update{upd("a")}, marker: 10},
		pollResult{updates: []model.Update{upd("b")}, marker: 11},
	)
	f.intake.err = context.Canceled

	require.NoError(t, f.poller.Run(f.ctx))
	assert.Equal(t, []int64{0}, f.src.markers, "при остановке marker не подтверждаем")
}

func TestRun_StopsOnCancelledContext(t *testing.T) {
	f := newFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- f.poller.Run(ctx) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run не завершился после отмены контекста")
	}
	assert.Empty(t, f.src.markers)
}
