package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/problem"
)

// ProblemRepo реализует usecase.ProblemRepo.
type ProblemRepo struct {
	pool *pgxpool.Pool
}

func NewProblemRepo(pool *pgxpool.Pool) *ProblemRepo {
	return &ProblemRepo{pool: pool}
}

// Lock — транзакционная advisory-блокировка на пару (чат, проблема):
// пока окна ещё нет, блокировать строку нечем. Снимается при коммите.
func (r *ProblemRepo) Lock(ctx context.Context, chatID int64, code problem.Code) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, fmt.Sprintf("problem:%d:%s", chatID, code))
	if err != nil {
		return fmt.Errorf("блокировка проблемы %s в чате %d: %w", code, chatID, err)
	}
	return nil
}

func (r *ProblemRepo) Current(ctx context.Context, chatID int64, code problem.Code, openSince time.Time) (problem.Incident, bool, error) {
	var i problem.Incident
	var problemCode string
	var notifiedAt *time.Time
	err := querierFrom(ctx, r.pool).QueryRow(ctx, `
		SELECT p.id, p.chat_id, p.problem, p.first_reported_at, p.notified, p.notified_at,
		       (SELECT count(*) FROM problem_report_users u WHERE u.report_id = p.id)
		FROM problem_reports p
		WHERE p.chat_id = $1 AND p.problem = $2 AND p.first_reported_at > $3
		ORDER BY p.first_reported_at DESC, p.id DESC
		LIMIT 1`, chatID, string(code), openSince,
	).Scan(&i.ID, &i.ChatID, &problemCode, &i.FirstReportedAt, &i.Notified, &notifiedAt, &i.Reporters)
	if errors.Is(err, pgx.ErrNoRows) {
		return problem.Incident{}, false, nil
	}
	if err != nil {
		return problem.Incident{}, false, fmt.Errorf("окно проблемы %s в чате %d: %w", code, chatID, err)
	}
	i.Problem = problem.Code(problemCode)
	if notifiedAt != nil {
		i.NotifiedAt = *notifiedAt
	}
	return i, true, nil
}

func (r *ProblemRepo) Create(ctx context.Context, i problem.Incident) (int64, error) {
	var id int64
	err := querierFrom(ctx, r.pool).QueryRow(ctx, `
		INSERT INTO problem_reports (chat_id, problem, first_reported_at, notified, notified_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		i.ChatID, string(i.Problem), i.FirstReportedAt, i.Notified, nullTime(i.NotifiedAt),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("новое окно проблемы %s в чате %d: %w", i.Problem, i.ChatID, err)
	}
	return id, nil
}

// AddReporter — житель в окне один раз: повтор упирается в первичный ключ.
func (r *ProblemRepo) AddReporter(ctx context.Context, incidentID, userID int64, at time.Time) (bool, error) {
	tag, err := querierFrom(ctx, r.pool).Exec(ctx, `
		INSERT INTO problem_report_users (report_id, user_id, reported_at)
		VALUES ($1, $2, $3) ON CONFLICT (report_id, user_id) DO NOTHING`,
		incidentID, userID, at)
	if err != nil {
		return false, fmt.Errorf("житель %d в окне проблемы %d: %w", userID, incidentID, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *ProblemRepo) SetNotified(ctx context.Context, incidentID int64, at time.Time) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx, `
		UPDATE problem_reports SET notified = $2, notified_at = $3 WHERE id = $1`,
		incidentID, !at.IsZero(), nullTime(at))
	if err != nil {
		return fmt.Errorf("отметка оповещения по окну %d: %w", incidentID, err)
	}
	return nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// ListChatAdmins реализует usecase.ChatAdmins: активные участники с ролью
// admin (роль записывается при добавлении бота).
func (r *MembershipRepo) ListChatAdmins(ctx context.Context, chatID int64) ([]membership.User, error) {
	rows, err := querierFrom(ctx, r.pool).Query(ctx, `
		SELECT u.id, u.nickname
		FROM memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.chat_id = $1 AND m.role = 'admin' AND m.status = 'active'
		ORDER BY u.id`, chatID)
	if err != nil {
		return nil, fmt.Errorf("админы чата %d: %w", chatID, err)
	}
	admins, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (membership.User, error) {
		var u membership.User
		err := row.Scan(&u.ID, &u.Nickname)
		return u, err
	})
	if err != nil {
		return nil, fmt.Errorf("админы чата %d: %w", chatID, err)
	}
	return admins, nil
}
