package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"maxhouse/internal/domain/rating"
	"maxhouse/internal/domain/vote"
)

// VoteRepo реализует usecase.VoteRepo.
type VoteRepo struct {
	pool *pgxpool.Pool
}

func NewVoteRepo(pool *pgxpool.Pool) *VoteRepo {
	return &VoteRepo{pool: pool}
}

// Upsert ставит голос и возвращает предыдущее значение. Предыдущее значение
// читается с блокировкой строки; вызывающий держит блокировку заявки,
// поэтому гонок между голосами одного пользователя нет.
func (r *VoteRepo) Upsert(ctx context.Context, requestID, userID int64, value vote.Value) (vote.Value, error) {
	q := querierFrom(ctx, r.pool)
	var prev string
	err := q.QueryRow(ctx, `
		WITH prev AS (
			SELECT value FROM votes WHERE request_id = $1 AND user_id = $2 FOR UPDATE
		), up AS (
			INSERT INTO votes (request_id, user_id, value) VALUES ($1, $2, $3)
			ON CONFLICT (request_id, user_id) DO UPDATE
				SET value = EXCLUDED.value, created_at = now()
				WHERE votes.value <> EXCLUDED.value
		)
		SELECT COALESCE((SELECT value FROM prev), '')`,
		requestID, userID, string(value)).Scan(&prev)
	if err != nil {
		return "", fmt.Errorf("голос (%d, %d): %w", requestID, userID, err)
	}
	return vote.Value(prev), nil
}

func (r *VoteRepo) Count(ctx context.Context, requestID int64) (confirms, refutes int, err error) {
	err = querierFrom(ctx, r.pool).QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE value = 'confirm'),
		       count(*) FILTER (WHERE value = 'refute')
		FROM votes WHERE request_id = $1`, requestID).Scan(&confirms, &refutes)
	if err != nil {
		return 0, 0, fmt.Errorf("подсчёт голосов заявки %d: %w", requestID, err)
	}
	return confirms, refutes, nil
}

// RatingRepo реализует usecase.RatingRepo.
type RatingRepo struct {
	pool *pgxpool.Pool
}

func NewRatingRepo(pool *pgxpool.Pool) *RatingRepo {
	return &RatingRepo{pool: pool}
}

// AddEvent пишет событие и обновляет кэш memberships.rating. Вызывать внутри
// TxManager.Do, чтобы событие и кэш менялись атомарно.
func (r *RatingRepo) AddEvent(ctx context.Context, e rating.Event) error {
	q := querierFrom(ctx, r.pool)
	var requestID *int64
	if e.RequestID != 0 {
		requestID = &e.RequestID
	}
	_, err := q.Exec(ctx, `
		INSERT INTO rating_events (chat_id, user_id, request_id, delta, reason)
		VALUES ($1, $2, $3, $4, $5)`,
		e.ChatID, e.UserID, requestID, e.Delta, string(e.Reason))
	if err != nil {
		return fmt.Errorf("событие рейтинга (%d, %d): %w", e.ChatID, e.UserID, err)
	}
	_, err = q.Exec(ctx, `
		UPDATE memberships SET rating = rating + $3 WHERE chat_id = $1 AND user_id = $2`,
		e.ChatID, e.UserID, e.Delta)
	if err != nil {
		return fmt.Errorf("кэш рейтинга (%d, %d): %w", e.ChatID, e.UserID, err)
	}
	return nil
}

// ResetNegative обнуляет отрицательные рейтинги и пишет события reset
// одним запросом: события и кэш меняются атомарно.
func (r *RatingRepo) ResetNegative(ctx context.Context) (int, error) {
	var n int
	err := querierFrom(ctx, r.pool).QueryRow(ctx, `
		WITH neg AS (
			SELECT chat_id, user_id, rating FROM memberships WHERE rating < 0 FOR UPDATE
		), ev AS (
			INSERT INTO rating_events (chat_id, user_id, delta, reason)
			SELECT chat_id, user_id, -rating, $1 FROM neg
		), upd AS (
			UPDATE memberships m SET rating = 0
			FROM neg WHERE m.chat_id = neg.chat_id AND m.user_id = neg.user_id
		)
		SELECT count(*) FROM neg`, string(rating.ReasonReset)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("сброс отрицательных рейтингов: %w", err)
	}
	return n, nil
}

// JobRuns реализует usecase.JobRuns на таблице scheduler_runs.
type JobRuns struct {
	pool *pgxpool.Pool
}

func NewJobRuns(pool *pgxpool.Pool) *JobRuns {
	return &JobRuns{pool: pool}
}

func (j *JobRuns) TryStart(ctx context.Context, job string, notAfter, now time.Time) (bool, error) {
	q := querierFrom(ctx, j.pool)
	_, err := q.Exec(ctx, `
		INSERT INTO scheduler_runs (job, last_run_at) VALUES ($1, 'epoch')
		ON CONFLICT (job) DO NOTHING`, job)
	if err != nil {
		return false, fmt.Errorf("журнал задачи %s: %w", job, err)
	}
	var last time.Time
	if err := q.QueryRow(ctx,
		`SELECT last_run_at FROM scheduler_runs WHERE job = $1 FOR UPDATE`, job).Scan(&last); err != nil {
		return false, fmt.Errorf("журнал задачи %s: %w", job, err)
	}
	if last.After(notAfter) {
		return false, nil
	}
	if _, err := q.Exec(ctx,
		`UPDATE scheduler_runs SET last_run_at = $2 WHERE job = $1`, job, now); err != nil {
		return false, fmt.Errorf("журнал задачи %s: %w", job, err)
	}
	return true, nil
}
