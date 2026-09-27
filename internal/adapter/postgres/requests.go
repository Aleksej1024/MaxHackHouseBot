package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/usecase"
)

// RequestRepo реализует usecase.RequestRepo.
type RequestRepo struct {
	pool *pgxpool.Pool
}

func NewRequestRepo(pool *pgxpool.Pool) *RequestRepo {
	return &RequestRepo{pool: pool}
}

func (r *RequestRepo) CountCreatedSince(ctx context.Context, chatID, authorID int64, typ requesttype.Code, since time.Time) (int, error) {
	var n int
	err := querierFrom(ctx, r.pool).QueryRow(ctx, `
		SELECT count(*) FROM requests
		WHERE chat_id = $1 AND author_id = $2 AND type = $3 AND created_at >= $4`,
		chatID, authorID, string(typ), since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("подсчёт заявок (%d, %d): %w", chatID, authorID, err)
	}
	return n, nil
}

// Create вставляет заявку и вложения. Вызывать внутри TxManager.Do,
// чтобы заявка и вложения сохранялись атомарно.
func (r *RequestRepo) Create(ctx context.Context, req request.Request, atts []request.Attachment) (int64, error) {
	q := querierFrom(ctx, r.pool)
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO requests (chat_id, author_id, type, status, is_anonymous, body,
		                      created_at, voting_ends_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`,
		req.ChatID, req.AuthorID, string(req.Type), string(req.Status), req.IsAnonymous, req.Body,
		req.CreatedAt, req.VotingEndsAt, req.ExpiresAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("вставка заявки: %w", err)
	}
	for _, a := range atts {
		_, err := q.Exec(ctx,
			`INSERT INTO attachments (request_id, kind, max_ref) VALUES ($1, $2, $3)`,
			id, string(a.Kind), a.Ref)
		if err != nil {
			return 0, fmt.Errorf("вставка вложения заявки %d: %w", id, err)
		}
	}
	return id, nil
}

func (r *RequestRepo) SetChatMessage(ctx context.Context, requestID int64, messageID string) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx,
		`UPDATE requests SET chat_message_id = $2, chat_sync = 'ok' WHERE id = $1`,
		requestID, messageID)
	if err != nil {
		return fmt.Errorf("chat_message_id заявки %d: %w", requestID, err)
	}
	return nil
}

func (r *RequestRepo) SetChatSyncFailed(ctx context.Context, requestID int64) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx,
		`UPDATE requests SET chat_sync = 'failed' WHERE id = $1`, requestID)
	if err != nil {
		return fmt.Errorf("chat_sync заявки %d: %w", requestID, err)
	}
	return nil
}

const requestColumns = `r.id, r.chat_id, r.author_id, r.type, r.status, r.is_anonymous, r.body,
	COALESCE(r.chat_message_id, ''), COALESCE(r.result_text, ''),
	r.created_at, r.voting_ends_at, r.expires_at, r.closed_at`

// requestDest — адреса для Scan в порядке requestColumns.
type requestDest struct {
	r        request.Request
	typ      string
	status   string
	closedAt *time.Time
}

func (d *requestDest) targets() []any {
	return []any{&d.r.ID, &d.r.ChatID, &d.r.AuthorID, &d.typ, &d.status, &d.r.IsAnonymous, &d.r.Body,
		&d.r.ChatMessageID, &d.r.ResultText, &d.r.CreatedAt, &d.r.VotingEndsAt, &d.r.ExpiresAt, &d.closedAt}
}

func (d *requestDest) request() request.Request {
	d.r.Type = requesttype.Code(d.typ)
	d.r.Status = request.Status(d.status)
	if d.closedAt != nil {
		d.r.ClosedAt = *d.closedAt
	}
	return d.r
}

func scanRequest(row pgx.Row) (request.Request, error) {
	var d requestDest
	err := row.Scan(d.targets()...)
	return d.request(), err
}

// GetForUpdate возвращает заявку и блокирует её строку до конца транзакции.
func (r *RequestRepo) GetForUpdate(ctx context.Context, requestID int64) (request.Request, error) {
	req, err := scanRequest(querierFrom(ctx, r.pool).QueryRow(ctx,
		`SELECT `+requestColumns+` FROM requests r WHERE r.id = $1 FOR UPDATE`, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return request.Request{}, fmt.Errorf("заявка %d: %w", requestID, usecase.ErrNotFound)
	}
	if err != nil {
		return request.Request{}, fmt.Errorf("заявка %d: %w", requestID, err)
	}
	return req, nil
}

// ClaimRating ставит rating_applied = true, только если флаг ещё не стоял.
func (r *RequestRepo) ClaimRating(ctx context.Context, requestID int64) (bool, error) {
	tag, err := querierFrom(ctx, r.pool).Exec(ctx,
		`UPDATE requests SET rating_applied = true WHERE id = $1 AND NOT rating_applied`, requestID)
	if err != nil {
		return false, fmt.Errorf("rating_applied заявки %d: %w", requestID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// LoadCard реализует usecase.CardReader. Ник автора анонимной заявки
// не читается вовсе.
func (r *RequestRepo) LoadCard(ctx context.Context, requestID int64) (usecase.RequestCard, error) {
	q := querierFrom(ctx, r.pool)
	var card usecase.RequestCard
	var d requestDest
	err := q.QueryRow(ctx, `
		SELECT `+requestColumns+`,
		       CASE WHEN r.is_anonymous THEN '' ELSE u.nickname END,
		       (SELECT count(*) FROM votes v WHERE v.request_id = r.id AND v.value = 'confirm'),
		       (SELECT count(*) FROM votes v WHERE v.request_id = r.id AND v.value = 'refute'),
		       (SELECT count(*) FROM comments c WHERE c.request_id = r.id)
		FROM requests r
		JOIN users u ON u.id = r.author_id
		WHERE r.id = $1`, requestID,
	).Scan(append(d.targets(), &card.AuthorNickname, &card.Confirms, &card.Refutes, &card.Materials)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return usecase.RequestCard{}, fmt.Errorf("карточка заявки %d: %w", requestID, usecase.ErrNotFound)
	}
	if err != nil {
		return usecase.RequestCard{}, fmt.Errorf("карточка заявки %d: %w", requestID, err)
	}
	card.Request = d.request()

	rows, err := q.Query(ctx, `
		SELECT kind, max_ref FROM attachments
		WHERE request_id = $1 AND comment_id IS NULL
		ORDER BY id`, requestID)
	if err != nil {
		return usecase.RequestCard{}, fmt.Errorf("вложения заявки %d: %w", requestID, err)
	}
	card.Attachments, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (request.Attachment, error) {
		var a request.Attachment
		var kind string
		err := row.Scan(&kind, &a.Ref)
		a.Kind = request.AttachmentKind(kind)
		return a, err
	})
	if err != nil {
		return usecase.RequestCard{}, fmt.Errorf("вложения заявки %d: %w", requestID, err)
	}
	return card, nil
}

// nextLocked блокирует одну заявку по условию; занятые другими репликами
// строки пропускаются (FOR UPDATE SKIP LOCKED).
func (r *RequestRepo) nextLocked(ctx context.Context, where, order string, now time.Time) (request.Request, bool, error) {
	req, err := scanRequest(querierFrom(ctx, r.pool).QueryRow(ctx,
		`SELECT `+requestColumns+` FROM requests r WHERE `+where+
			` ORDER BY `+order+` LIMIT 1 FOR UPDATE SKIP LOCKED`, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return request.Request{}, false, nil
	}
	if err != nil {
		return request.Request{}, false, fmt.Errorf("выборка заявок планировщика: %w", err)
	}
	return req, true, nil
}

func (r *RequestRepo) NextVotingEnded(ctx context.Context, now time.Time) (request.Request, bool, error) {
	return r.nextLocked(ctx,
		`r.status = 'open' AND r.voting_ends_at <= $1 AND r.expires_at > $1`, `r.voting_ends_at, r.id`, now)
}

func (r *RequestRepo) NextExpired(ctx context.Context, now time.Time) (request.Request, bool, error) {
	return r.nextLocked(ctx,
		`r.status IN ('open', 'in_progress') AND r.expires_at <= $1`, `r.expires_at, r.id`, now)
}

func (r *RequestRepo) SaveState(ctx context.Context, req request.Request) error {
	var closedAt *time.Time
	if !req.ClosedAt.IsZero() {
		closedAt = &req.ClosedAt
	}
	_, err := querierFrom(ctx, r.pool).Exec(ctx, `
		UPDATE requests SET status = $2, result_text = NULLIF($3, ''), closed_at = $4
		WHERE id = $1`, req.ID, string(req.Status), req.ResultText, closedAt)
	if err != nil {
		return fmt.Errorf("статус заявки %d: %w", req.ID, err)
	}
	return nil
}

func (r *RequestRepo) UpdateBody(ctx context.Context, requestID int64, body string) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx,
		`UPDATE requests SET body = $2 WHERE id = $1`, requestID, body)
	if err != nil {
		return fmt.Errorf("текст заявки %d: %w", requestID, err)
	}
	return nil
}

func (r *RequestRepo) ListByAuthor(ctx context.Context, chatID, authorID int64, limit int) ([]request.Request, error) {
	rows, err := querierFrom(ctx, r.pool).Query(ctx, `
		SELECT `+requestColumns+` FROM requests r
		WHERE r.chat_id = $1 AND r.author_id = $2 AND r.status <> 'deleted'
		ORDER BY r.created_at DESC, r.id DESC
		LIMIT $3`, chatID, authorID, limit)
	if err != nil {
		return nil, fmt.Errorf("заявки автора (%d, %d): %w", chatID, authorID, err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (request.Request, error) {
		return scanRequest(row)
	})
	if err != nil {
		return nil, fmt.Errorf("заявки автора (%d, %d): %w", chatID, authorID, err)
	}
	return list, nil
}

// ListActiveInChat реализует usecase.RequestBrowser.
func (r *RequestRepo) ListActiveInChat(ctx context.Context, chatID int64, limit int) ([]request.Request, error) {
	rows, err := querierFrom(ctx, r.pool).Query(ctx, `
		SELECT `+requestColumns+` FROM requests r
		WHERE r.chat_id = $1 AND r.status IN ('open', 'in_progress')
		ORDER BY r.created_at DESC, r.id DESC
		LIMIT $2`, chatID, limit)
	if err != nil {
		return nil, fmt.Errorf("заявки чата %d: %w", chatID, err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (request.Request, error) {
		return scanRequest(row)
	})
	if err != nil {
		return nil, fmt.Errorf("заявки чата %d: %w", chatID, err)
	}
	return list, nil
}
