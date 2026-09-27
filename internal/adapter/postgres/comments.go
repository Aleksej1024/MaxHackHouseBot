package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"maxhouse/internal/domain/broadcast"
	"maxhouse/internal/domain/comment"
	"maxhouse/internal/domain/request"
)

// CommentRepo реализует usecase.CommentRepo.
type CommentRepo struct {
	pool *pgxpool.Pool
}

func NewCommentRepo(pool *pgxpool.Pool) *CommentRepo {
	return &CommentRepo{pool: pool}
}

// Add сохраняет материал и вложения (с request_id и comment_id). Вызывать
// внутри TxManager.Do.
func (r *CommentRepo) Add(ctx context.Context, c comment.Comment) (int64, error) {
	q := querierFrom(ctx, r.pool)
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO comments (request_id, author_id, kind, body, created_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		c.RequestID, c.AuthorID, string(c.Kind), c.Body, c.CreatedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("вставка материала к заявке %d: %w", c.RequestID, err)
	}
	for _, a := range c.Attachments {
		_, err := q.Exec(ctx,
			`INSERT INTO attachments (request_id, comment_id, kind, max_ref) VALUES ($1, $2, $3, $4)`,
			c.RequestID, id, string(a.Kind), a.Ref)
		if err != nil {
			return 0, fmt.Errorf("вложение материала %d: %w", id, err)
		}
	}
	return id, nil
}

// List — страница материалов по времени с ником отправителя и вложениями.
func (r *CommentRepo) List(ctx context.Context, requestID int64, offset, limit int) ([]comment.Comment, error) {
	q := querierFrom(ctx, r.pool)
	rows, err := q.Query(ctx, `
		SELECT c.id, c.request_id, c.author_id, u.nickname, c.kind, c.body, c.created_at
		FROM comments c
		JOIN users u ON u.id = c.author_id
		WHERE c.request_id = $1
		ORDER BY c.created_at, c.id
		OFFSET $2 LIMIT $3`, requestID, offset, limit)
	if err != nil {
		return nil, fmt.Errorf("материалы заявки %d: %w", requestID, err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (comment.Comment, error) {
		var c comment.Comment
		var kind string
		err := row.Scan(&c.ID, &c.RequestID, &c.AuthorID, &c.AuthorNickname, &kind, &c.Body, &c.CreatedAt)
		c.Kind = comment.Kind(kind)
		return c, err
	})
	if err != nil || len(list) == 0 {
		if err != nil {
			err = fmt.Errorf("материалы заявки %d: %w", requestID, err)
		}
		return list, err
	}

	ids := make([]int64, len(list))
	index := make(map[int64]int, len(list))
	for i, c := range list {
		ids[i], index[c.ID] = c.ID, i
	}
	rows, err = q.Query(ctx, `
		SELECT comment_id, kind, max_ref FROM attachments
		WHERE comment_id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return nil, fmt.Errorf("вложения материалов заявки %d: %w", requestID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var commentID int64
		var a request.Attachment
		var kind string
		if err := rows.Scan(&commentID, &kind, &a.Ref); err != nil {
			return nil, fmt.Errorf("вложения материалов заявки %d: %w", requestID, err)
		}
		a.Kind = request.AttachmentKind(kind)
		i := index[commentID]
		list[i].Attachments = append(list[i].Attachments, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("вложения материалов заявки %d: %w", requestID, err)
	}
	return list, nil
}

func (r *CommentRepo) Count(ctx context.Context, requestID int64) (int, error) {
	var n int
	err := querierFrom(ctx, r.pool).QueryRow(ctx,
		`SELECT count(*) FROM comments WHERE request_id = $1`, requestID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("число материалов заявки %d: %w", requestID, err)
	}
	return n, nil
}

// BroadcastRepo реализует usecase.BroadcastRepo.
type BroadcastRepo struct {
	pool *pgxpool.Pool
}

func NewBroadcastRepo(pool *pgxpool.Pool) *BroadcastRepo {
	return &BroadcastRepo{pool: pool}
}

func (r *BroadcastRepo) LastAt(ctx context.Context, requestID int64) (time.Time, bool, error) {
	var at *time.Time
	err := querierFrom(ctx, r.pool).QueryRow(ctx,
		`SELECT max(created_at) FROM broadcasts WHERE request_id = $1`, requestID).Scan(&at)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("последняя рассылка заявки %d: %w", requestID, err)
	}
	if at == nil {
		return time.Time{}, false, nil
	}
	return *at, true, nil
}

func (r *BroadcastRepo) Record(ctx context.Context, requestID int64, audience broadcast.Audience, recipients int, at time.Time) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx, `
		INSERT INTO broadcasts (request_id, audience, recipients, created_at)
		VALUES ($1, $2, $3, $4)`, requestID, string(audience), recipients, at)
	if err != nil {
		return fmt.Errorf("запись рассылки заявки %d: %w", requestID, err)
	}
	return nil
}

// Recipients — проголосовавшие из аудитории, которые всё ещё активные
// участники чата заявки и доступны в личке.
func (r *BroadcastRepo) Recipients(ctx context.Context, requestID int64, audience broadcast.Audience) ([]int64, error) {
	values := make([]string, 0, 2)
	for _, v := range audience.Votes() {
		values = append(values, string(v))
	}
	rows, err := querierFrom(ctx, r.pool).Query(ctx, `
		SELECT v.user_id
		FROM votes v
		JOIN requests rq ON rq.id = v.request_id
		JOIN memberships m ON m.chat_id = rq.chat_id AND m.user_id = v.user_id AND m.status = 'active'
		JOIN users u ON u.id = v.user_id AND NOT u.dm_unavailable
		WHERE v.request_id = $1 AND v.value = ANY($2)
		ORDER BY v.user_id`, requestID, values)
	if err != nil {
		return nil, fmt.Errorf("получатели рассылки заявки %d: %w", requestID, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("получатели рассылки заявки %d: %w", requestID, err)
	}
	return ids, nil
}
