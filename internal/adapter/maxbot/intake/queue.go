package intake

import (
	"context"
	"hash/fnv"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

// Queue — in-memory очередь апдейтов, шардированная по пользователю.
// Число шардов равно числу воркеров, каждый шард обслуживает один
// воркер, поэтому апдейты одного пользователя обрабатываются
// строго последовательно.
//
// Обработчики работают на контексте очереди, а не на контексте запроса:
// остановка приёма не обрывает начатую обработку. Контекст очереди
// отменяется, только когда истекает время остановки (Shutdown).
type Queue struct {
	processor Processor
	log       *slog.Logger
	shards    []chan model.Update
	wg        sync.WaitGroup
	closeOnce sync.Once
	work      context.Context
	abort     context.CancelFunc
	dropped   atomic.Int64
}

func NewQueue(processor Processor, workers, buffer int, log *slog.Logger) *Queue {
	work, abort := context.WithCancel(context.Background())
	q := &Queue{
		processor: processor,
		log:       log,
		shards:    make([]chan model.Update, workers),
		work:      work,
		abort:     abort,
	}
	for i := range q.shards {
		q.shards[i] = make(chan model.Update, buffer)
	}
	return q
}

// Start запускает воркеров.
func (q *Queue) Start() {
	for i := range q.shards {
		q.wg.Add(1)
		go func(ch <-chan model.Update) {
			defer q.wg.Done()
			for upd := range ch {
				// После истечения времени остановки оставшиеся апдейты
				// не обрабатываем, только считаем.
				if q.work.Err() != nil {
					q.dropped.Add(1)
					continue
				}
				q.processor.Process(q.work, upd)
			}
		}(q.shards[i])
	}
}

// Enqueue кладёт апдейт в шард. Не блокирует: при переполнении
// возвращает false, чтобы handler MAX успел ответить 200 OK.
func (q *Queue) Enqueue(upd model.Update) bool {
	shard := q.shardFor(shardKey(upd))
	select {
	case q.shards[shard] <- upd:
		return true
	default:
		return false
	}
}

// EnqueueWait кладёт апдейт в шард, ожидая места в буфере (long polling:
// лучше притормозить опрос, чем потерять апдейт). Ошибка — только отмена ctx.
func (q *Queue) EnqueueWait(ctx context.Context, upd model.Update) error {
	shard := q.shardFor(shardKey(upd))
	select {
	case q.shards[shard] <- upd:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown закрывает шарды и ждёт, пока воркеры доработают очередь, но не
// дольше ctx (SHUTDOWN_TIMEOUT). По истечении времени отменяет контекст
// обработки: начатые транзакции откатываются, вызовы MAX прерываются,
// оставшиеся апдейты отбрасываются. Возвращает число отброшенных апдейтов.
// Вызывать после остановки приёма новых апдейтов.
func (q *Queue) Shutdown(ctx context.Context) int {
	q.closeOnce.Do(func() {
		for _, ch := range q.shards {
			close(ch)
		}
	})
	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		q.abort()
		<-done // обработчики уважают контекст и быстро завершаются
	}
	q.abort()
	return int(q.dropped.Load())
}

func (q *Queue) shardFor(key int64) int {
	h := fnv.New64a()
	var b [8]byte
	for i := range b {
		b[i] = byte(key >> (8 * i))
	}
	_, _ = h.Write(b[:])
	return int(h.Sum64() % uint64(len(q.shards)))
}
