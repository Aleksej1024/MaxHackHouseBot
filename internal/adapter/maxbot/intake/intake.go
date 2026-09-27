// Package intake — приём обновлений MAX независимо от источника (webhook
// или long polling, антидубль в Redis и очередь,
// шардированная по пользователю. Обновления одного пользователя
// обрабатываются строго последовательно, тяжёлая работа — в ограниченном
// пуле воркеров.
//
// Очередь in-memory: при перезапуске процесса необработанные апдейты
// теряются; MAX пришлёт их повторно (webhook) или отдаст снова как
// неподтверждённые (long polling).
package intake

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

const (
	defaultWorkers     = 16
	defaultQueueBuffer = 1024
)

// Processor обрабатывает апдейт из очереди. Реализации должны быть идемпотентны.
type Processor interface {
	Process(ctx context.Context, upd model.Update)
}

// ProcessorFunc адаптирует функцию к Processor.
type ProcessorFunc func(ctx context.Context, upd model.Update)

func (f ProcessorFunc) Process(ctx context.Context, upd model.Update) { f(ctx, upd) }

// DedupStore — хранилище ключей антидубля (Redis SET NX с TTL).
type DedupStore interface {
	SetNX(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// Options — зависимости приёма обновлений.
type Options struct {
	Processor   Processor
	Dedup       DedupStore
	DedupTTL    time.Duration
	Workers     int
	QueueBuffer int
	Log         *slog.Logger
}

// Intake — антидубль и очередь обновлений.
type Intake struct {
	dedup DedupStore
	ttl   time.Duration
	queue *Queue
	log   *slog.Logger
}

func New(opts Options) *Intake {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	buffer := opts.QueueBuffer
	if buffer <= 0 {
		buffer = defaultQueueBuffer
	}
	return &Intake{
		dedup: opts.Dedup,
		ttl:   opts.DedupTTL,
		queue: NewQueue(opts.Processor, workers, buffer, log),
		log:   log,
	}
}

// Start запускает воркеров очереди.
func (in *Intake) Start() { in.queue.Start() }

// Shutdown дожидается обработки принятых апдейтов, но не дольше ctx.
// Вызывать после остановки всех источников (HTTP-сервера и опроса).
// Не успевшие обработаться апдейты отбрасываются: webhook MAX уже получил
// 200 OK, в режиме long polling они уже подтверждены marker'ом.
func (in *Intake) Shutdown(ctx context.Context) {
	if dropped := in.queue.Shutdown(ctx); dropped > 0 {
		in.log.Error("время остановки истекло, часть апдейтов не обработана", "dropped", dropped)
	}
}

// Accept — для webhook: антидубль и постановка в очередь без ожидания,
// чтобы успеть ответить MAX 200 OK. При переполнении апдейт отбрасывается.
func (in *Intake) Accept(ctx context.Context, upd model.Update) {
	key, fresh := in.checkDedup(ctx, upd)
	if !fresh {
		return
	}
	if !in.queue.Enqueue(upd) {
		in.log.Error("очередь апдейтов переполнена, апдейт отброшен",
			"update_type", string(upd.UpdateType), "request_id", key)
	}
}

// AcceptWait — для long polling: антидубль и постановка в очередь
// с ожиданием места. Ошибка — только отмена контекста.
func (in *Intake) AcceptWait(ctx context.Context, upd model.Update) error {
	if _, fresh := in.checkDedup(ctx, upd); !fresh {
		return nil
	}
	return in.queue.EnqueueWait(ctx, upd)
}

// checkDedup возвращает ключ апдейта и признак, что он новый. Если Redis
// недоступен, апдейт обрабатывается (обработчики идемпотентны).
func (in *Intake) checkDedup(ctx context.Context, upd model.Update) (string, bool) {
	key := UpdateKey(upd)
	fresh, err := in.dedup.SetNX(ctx, key, in.ttl)
	switch {
	case err != nil:
		in.log.Error("антидубль недоступен, обрабатываем апдейт повторно-небезопасно",
			"update_type", string(upd.UpdateType), "request_id", key, "error", err)
		return key, true
	case !fresh:
		in.log.Debug("дубликат апдейта, пропуск", "update_type", string(upd.UpdateType), "request_id", key)
		return key, false
	}
	return key, true
}

// UpdateKey — ключ антидубля и request_id для логов:
// dedup:{update_type}:{callback_id|mid|chat_id:user_id}:{timestamp}.
func UpdateKey(u model.Update) string {
	var id string
	switch {
	case u.Callback != nil && u.Callback.CallbackID != "":
		id = u.Callback.CallbackID
	case u.MessageID != "":
		id = u.MessageID
	default:
		id = fmt.Sprintf("%d:%d", u.ChatID, u.UserID)
	}
	return fmt.Sprintf("dedup:%s:%s:%d", u.UpdateType, id, u.Timestamp)
}

// shardKey выбирает шард очереди: апдейты одного пользователя
// обрабатываются строго последовательно. Для callback берём автора
// нажатия, для остальных апдейтов — UserID; если его нет (события чата),
// шардируем по ChatID.
func shardKey(u model.Update) int64 {
	if u.UpdateType == model.UpdateMessageCallback && u.Callback != nil {
		return u.Callback.User.UserID
	}
	if u.UserID != 0 {
		return u.UserID
	}
	return u.ChatID
}
