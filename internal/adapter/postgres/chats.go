package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/usecase"
)

// ChatRepo реализует usecase.ChatRepo.
type ChatRepo struct {
	pool *pgxpool.Pool
}

func NewChatRepo(pool *pgxpool.Pool) *ChatRepo {
	return &ChatRepo{pool: pool}
}

func (r *ChatRepo) Upsert(ctx context.Context, chat housechat.Chat) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx, `
		INSERT INTO house_chats (id, title, status) VALUES ($1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET title = EXCLUDED.title`,
		chat.ID, chat.Title, string(chat.Status))
	if err != nil {
		return fmt.Errorf("upsert чата %d: %w", chat.ID, err)
	}
	return nil
}

func (r *ChatRepo) Get(ctx context.Context, chatID int64) (housechat.Chat, error) {
	var chat housechat.Chat
	var status string
	err := querierFrom(ctx, r.pool).QueryRow(ctx,
		`SELECT id, title, status FROM house_chats WHERE id = $1`, chatID,
	).Scan(&chat.ID, &chat.Title, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return housechat.Chat{}, fmt.Errorf("чат %d: %w", chatID, usecase.ErrNotFound)
	}
	if err != nil {
		return housechat.Chat{}, fmt.Errorf("чат %d: %w", chatID, err)
	}
	chat.Status = housechat.Status(status)
	return chat, nil
}

func (r *ChatRepo) SetStatus(ctx context.Context, chatID int64, status housechat.Status) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx,
		`UPDATE house_chats SET status = $2 WHERE id = $1`, chatID, string(status))
	if err != nil {
		return fmt.Errorf("статус чата %d: %w", chatID, err)
	}
	return nil
}
