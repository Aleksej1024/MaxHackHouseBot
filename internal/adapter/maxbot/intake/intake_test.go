package intake

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDedup struct {
	mu   sync.Mutex
	seen map[string]bool
	err  error
}

func newFakeDedup() *fakeDedup { return &fakeDedup{seen: make(map[string]bool)} }

func (f *fakeDedup) SetNX(_ context.Context, key string, _ time.Duration) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen[key] {
		return false, nil
	}
	f.seen[key] = true
	return true, nil
}

type recorder struct{ ch chan model.Update }

func newRecorder() *recorder { return &recorder{ch: make(chan model.Update, 16)} }

func (r *recorder) Process(_ context.Context, upd model.Update) { r.ch <- upd }

func (r *recorder) receive(t *testing.T) model.Update {
	t.Helper()
	select {
	case upd := <-r.ch:
		return upd
	case <-time.After(5 * time.Second):
		t.Fatal("апдейт не дошёл до обработчика")
		return model.Update{}
	}
}

func (r *recorder) receiveNone(t *testing.T) {
	t.Helper()
	select {
	case upd := <-r.ch:
		t.Fatalf("лишний апдейт дошёл до обработчика: %v", upd.UpdateType)
	case <-time.After(100 * time.Millisecond):
	}
}

func newTestIntake(t *testing.T, dedup DedupStore, proc Processor) *Intake {
	t.Helper()
	in := New(Options{
		Processor: proc,
		Dedup:     dedup,
		DedupTTL:  time.Hour,
		Workers:   2,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	in.Start()
	t.Cleanup(func() { in.Shutdown(context.Background()) })
	return in
}

var msg = model.Update{UpdateType: model.UpdateMessageCreated, UserID: 42, MessageID: "mid.1", Timestamp: 1700000000}

func TestAccept_Dedup(t *testing.T) {
	rec := newRecorder()
	in := newTestIntake(t, newFakeDedup(), rec)

	in.Accept(context.Background(), msg)
	rec.receive(t)

	in.Accept(context.Background(), msg)
	rec.receiveNone(t)
}

func TestAcceptWait_Dedup(t *testing.T) {
	rec := newRecorder()
	in := newTestIntake(t, newFakeDedup(), rec)

	require.NoError(t, in.AcceptWait(context.Background(), msg))
	rec.receive(t)
	require.NoError(t, in.AcceptWait(context.Background(), msg))
	rec.receiveNone(t)
}

func TestAccept_DedupStoreError_FailOpen(t *testing.T) {
	dedup := newFakeDedup()
	dedup.err = errors.New("redis down")
	rec := newRecorder()
	in := newTestIntake(t, dedup, rec)

	in.Accept(context.Background(), msg)
	rec.receive(t)
}

func TestEnqueue_FullQueue(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	proc := ProcessorFunc(func(context.Context, model.Update) {
		started <- struct{}{}
		<-block
	})
	q := NewQueue(proc, 1, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	q.Start()
	t.Cleanup(func() { close(block); q.Shutdown(context.Background()) })

	require.True(t, q.Enqueue(msg))
	<-started // воркер занят первым апдейтом
	require.True(t, q.Enqueue(msg), "второй — в буфер")
	assert.False(t, q.Enqueue(msg), "webhook: при переполнении не блокируемся")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, q.EnqueueWait(ctx, msg), context.Canceled, "long polling: ждём места до отмены")
}

func TestQueue_SameUserSequential(t *testing.T) {
	var mu sync.Mutex
	var order []string
	done := make(chan struct{}, 3)
	proc := ProcessorFunc(func(_ context.Context, u model.Update) {
		mu.Lock()
		order = append(order, u.MessageID)
		mu.Unlock()
		done <- struct{}{}
	})
	q := NewQueue(proc, 4, 8, slog.New(slog.NewTextHandler(io.Discard, nil)))
	q.Start()
	for _, id := range []string{"a", "b", "c"} {
		u := msg
		u.MessageID = id
		require.NoError(t, q.EnqueueWait(context.Background(), u))
	}
	for i := 0; i < 3; i++ {
		<-done
	}
	assert.Zero(t, q.Shutdown(context.Background()))
	assert.Equal(t, []string{"a", "b", "c"}, order, "апдейты одного пользователя — по порядку")
}

func TestShardKey(t *testing.T) {
	callback := model.Update{
		UpdateType: model.UpdateMessageCallback,
		Callback:   &model.Callback{User: model.User{UserID: 7}},
	}
	assert.Equal(t, int64(7), shardKey(callback))

	byUser := model.Update{UpdateType: model.UpdateMessageCreated, UserID: 42, ChatID: -100}
	assert.Equal(t, int64(42), shardKey(byUser))

	byChat := model.Update{UpdateType: model.UpdateBotAdded, ChatID: -100}
	assert.Equal(t, int64(-100), shardKey(byChat))
}

func TestUpdateKey(t *testing.T) {
	cb := model.Update{UpdateType: model.UpdateMessageCallback, MessageID: "mid.1", Timestamp: 5,
		Callback: &model.Callback{CallbackID: "cb1"}}
	assert.Equal(t, "dedup:message_callback:cb1:5", UpdateKey(cb), "у callback — id нажатия, а не сообщения")
	assert.Equal(t, "dedup:message_created:mid.1:1700000000", UpdateKey(msg))
	ev := model.Update{UpdateType: model.UpdateUserAdded, ChatID: -1, UserID: 2, Timestamp: 3}
	assert.Equal(t, "dedup:user_added:-1:2:3", UpdateKey(ev))
}

func TestShutdown_DrainsAcceptedUpdates(t *testing.T) {
	var mu sync.Mutex
	processed := 0
	proc := ProcessorFunc(func(context.Context, model.Update) {
		mu.Lock()
		processed++
		mu.Unlock()
	})
	q := NewQueue(proc, 2, 16, slog.New(slog.NewTextHandler(io.Discard, nil)))
	q.Start()
	for i := 0; i < 10; i++ {
		u := msg
		u.UserID = int64(i)
		require.True(t, q.Enqueue(u))
	}

	dropped := q.Shutdown(context.Background())

	assert.Zero(t, dropped)
	assert.Equal(t, 10, processed, "все принятые апдейты обработаны до выхода")
}

func TestShutdown_DeadlineAbortsAndDrops(t *testing.T) {
	started := make(chan struct{})
	sawCancel := make(chan bool, 1)
	proc := ProcessorFunc(func(ctx context.Context, _ model.Update) {
		select {
		case started <- struct{}{}:
			// Первый апдейт «завис» (медленный MAX) — ждём отмены контекста.
			<-ctx.Done()
			sawCancel <- true
		default:
			t.Error("после истечения времени апдейты не должны обрабатываться")
		}
	})
	q := NewQueue(proc, 1, 8, slog.New(slog.NewTextHandler(io.Discard, nil)))
	q.Start()
	for i := 0; i < 3; i++ {
		require.True(t, q.Enqueue(msg)) // один пользователь — один шард
	}
	<-started

	expired, cancel := context.WithCancel(context.Background())
	cancel() // SHUTDOWN_TIMEOUT уже истёк

	dropped := q.Shutdown(expired)

	assert.True(t, <-sawCancel, "обработчик получил отмену контекста и завершился")
	assert.Equal(t, 2, dropped, "необработанные апдейты отброшены и посчитаны")
}

func TestIntakeShutdown_ContextDoesNotLeakIntoProcessing(t *testing.T) {
	gotCtx := make(chan context.Context, 1)
	proc := ProcessorFunc(func(ctx context.Context, _ model.Update) { gotCtx <- ctx })
	in := newTestIntake(t, newFakeDedup(), proc)

	reqCtx, cancelReq := context.WithCancel(context.Background())
	in.Accept(reqCtx, msg)
	cancelReq() // HTTP-запрос webhook завершился
	ctx := <-gotCtx
	assert.NoError(t, ctx.Err(), "обработка идёт на контексте очереди, а не запроса")
}
