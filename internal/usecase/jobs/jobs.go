// Package jobs — задачи планировщика.
//
// Каждая заявка обрабатывается в своей транзакции: строка выбирается
// FOR UPDATE SKIP LOCKED, поэтому при нескольких репликах задачи
// не дублируются. Сообщение в чате обновляется после коммита.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"maxhouse/internal/domain/request"
	"maxhouse/internal/usecase"
)

// RatingApplier начисляет рейтинг по итогам голосования (voting.Rater).
type RatingApplier interface {
	Apply(ctx context.Context, req request.Request) (int, error)
}

// ChatSyncer обновляет карточку заявки в чате (chatsync.Syncer).
type ChatSyncer interface {
	Sync(ctx context.Context, requestID int64) bool
}

// LifecycleDeps — зависимости задач жизненного цикла заявок.
type LifecycleDeps struct {
	Requests usecase.RequestLifecycleRepo
	Rater    RatingApplier
	Sync     ChatSyncer
	Tx       usecase.TxManager
	Clock    usecase.Clock
	Log      *slog.Logger
}

// Lifecycle — автоматические переходы статусов заявок.
type Lifecycle struct {
	LifecycleDeps
	batch int
}

// NewLifecycle; batch — сколько заявок обрабатывать за один тик
// (SCHEDULER_BATCH_SIZE), остальные дождутся следующего.
func NewLifecycle(deps LifecycleDeps, batch int) *Lifecycle {
	if batch <= 0 {
		batch = 1
	}
	return &Lifecycle{LifecycleDeps: deps, batch: batch}
}

// Run выполняет один проход: сначала истечение срока (оно поглощает
// окончание голосования), затем окончание голосования.
func (l *Lifecycle) Run(ctx context.Context) error {
	if _, err := l.ExpireRequests(ctx); err != nil {
		return err
	}
	_, err := l.EndVoting(ctx)
	return err
}

// EndVoting переводит open → in_progress, когда прошёл срок голосования,
// и начисляет рейтинг. Возвращает число обработанных заявок.
func (l *Lifecycle) EndVoting(ctx context.Context) (int, error) {
	return l.process(ctx, "окончание голосования", l.Requests.NextVotingEnded, func(r *request.Request) error {
		return r.EndVoting()
	})
}

// ExpireRequests переводит open/in_progress в expired по истечении общего
// срока. Если шло голосование, оно завершается и рейтинг считается сейчас.
func (l *Lifecycle) ExpireRequests(ctx context.Context) (int, error) {
	return l.process(ctx, "истечение срока", l.Requests.NextExpired, func(r *request.Request) error {
		_, err := r.Expire(l.Clock.Now())
		return err
	})
}

type nextFunc func(ctx context.Context, now time.Time) (request.Request, bool, error)

func (l *Lifecycle) process(ctx context.Context, name string, next nextFunc, apply func(r *request.Request) error) (int, error) {
	n := 0
	// Остановка бота прерывает пачку: оставшиеся заявки обработает следующий тик.
	for n < l.batch && ctx.Err() == nil {
		var req request.Request
		var found bool
		err := l.Tx.Do(ctx, func(ctx context.Context) error {
			var err error
			req, found, err = next(ctx, l.Clock.Now())
			if err != nil || !found {
				return err
			}
			if err := apply(&req); err != nil {
				return err
			}
			if err := l.Requests.SaveState(ctx, req); err != nil {
				return err
			}
			// Идемпотентно: для in_progress рейтинг уже посчитан.
			_, err = l.Rater.Apply(ctx, req)
			return err
		})
		if err != nil {
			return n, fmt.Errorf("%s: заявка %d: %w", name, req.ID, err)
		}
		if !found {
			return n, nil
		}
		n++
		l.Log.Info("статус заявки изменён планировщиком",
			"request_id", req.ID, "chat_id", req.ChatID, "status", string(req.Status))
		l.Sync.Sync(ctx, req.ID)
	}
	return n, nil
}

// RatingResetJob — имя задачи сброса рейтингов в журнале запусков.
const RatingResetJob = "rating_reset"

// RatingReset обнуляет отрицательные рейтинги не чаще раза в
// RATING_RESET_INTERVAL и пишет события reset. Задача вызывается на каждом
// тике планировщика, но сбрасывает, только когда с прошлого сброса прошёл
// интервал (журнал scheduler_runs); иначе — одна быстрая проверка.
type RatingReset struct {
	runs     usecase.JobRuns
	ratings  usecase.RatingResetRepo
	interval time.Duration
	tx       usecase.TxManager
	clock    usecase.Clock
	log      *slog.Logger
}

func NewRatingReset(runs usecase.JobRuns, ratings usecase.RatingResetRepo, interval time.Duration,
	tx usecase.TxManager, clock usecase.Clock, log *slog.Logger) *RatingReset {
	return &RatingReset{runs: runs, ratings: ratings, interval: interval, tx: tx, clock: clock, log: log}
}

// Run выполняет сброс, если с прошлого прошло не меньше интервала.
// Первый запуск (записи в журнале нет) сбрасывает сразу.
func (r *RatingReset) Run(ctx context.Context) error {
	now := r.clock.Now()
	var started bool
	var n int
	err := r.tx.Do(ctx, func(ctx context.Context) error {
		var err error
		started, err = r.runs.TryStart(ctx, RatingResetJob, now.Add(-r.interval), now)
		if err != nil || !started {
			return err
		}
		n, err = r.ratings.ResetNegative(ctx)
		return err
	})
	if err != nil {
		return fmt.Errorf("сброс рейтингов: %w", err)
	}
	if started {
		r.log.Info("отрицательные рейтинги сброшены", "memberships", n, "interval", r.interval)
	}
	return nil
}
