package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/usecase"
)

// MembershipRepo реализует usecase.MembershipRepo.
type MembershipRepo struct {
	pool *pgxpool.Pool
}

func NewMembershipRepo(pool *pgxpool.Pool) *MembershipRepo {
	return &MembershipRepo{pool: pool}
}

func (r *MembershipRepo) UpsertUser(ctx context.Context, u membership.User) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx, `
		INSERT INTO users (id, nickname) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET nickname = EXCLUDED.nickname`,
		u.ID, u.Nickname)
	if err != nil {
		return fmt.Errorf("upsert пользователя %d: %w", u.ID, err)
	}
	return nil
}

func (r *MembershipRepo) UpsertMembership(ctx context.Context, m membership.Membership) error {
	// При конфликте только status = active: rating, role и joined_at
	// сохраняются, повторный вход не обнуляет рейтинг (раздел 6).
	_, err := querierFrom(ctx, r.pool).Exec(ctx, `
		INSERT INTO memberships (chat_id, user_id, role, rating, status)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (chat_id, user_id) DO UPDATE SET status = 'active'`,
		m.ChatID, m.UserID, string(m.Role), m.Rating, string(m.Status))
	if err != nil {
		return fmt.Errorf("upsert участия (%d, %d): %w", m.ChatID, m.UserID, err)
	}
	return nil
}

func (r *MembershipRepo) SetStatus(ctx context.Context, chatID, userID int64, status membership.Status) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx,
		`UPDATE memberships SET status = $3 WHERE chat_id = $1 AND user_id = $2`,
		chatID, userID, string(status))
	if err != nil {
		return fmt.Errorf("статус участия (%d, %d): %w", chatID, userID, err)
	}
	return nil
}

// UpdateNickname обновляет ник только у существующего пользователя:
// незнакомых (не состоящих в чатах) пользователей не сохраняем (раздел 3, п. 8).
// Пользователь сам написал боту — значит, личка снова доступна.
func (r *MembershipRepo) UpdateNickname(ctx context.Context, userID int64, nickname string) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx, `
		UPDATE users SET nickname = $2, dm_unavailable = false
		WHERE id = $1 AND (nickname <> $2 OR dm_unavailable)`, userID, nickname)
	if err != nil {
		return fmt.Errorf("ник пользователя %d: %w", userID, err)
	}
	return nil
}

// ListActiveChats возвращает активные чаты, где пользователь — активный участник.
func (r *MembershipRepo) ListActiveChats(ctx context.Context, userID int64) ([]housechat.Chat, error) {
	rows, err := querierFrom(ctx, r.pool).Query(ctx, `
		SELECT c.id, c.title, c.status
		FROM memberships m
		JOIN house_chats c ON c.id = m.chat_id
		WHERE m.user_id = $1 AND m.status = 'active' AND c.status = 'active'
		ORDER BY c.title, c.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("чаты пользователя %d: %w", userID, err)
	}
	chats, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (housechat.Chat, error) {
		var c housechat.Chat
		var status string
		err := row.Scan(&c.ID, &c.Title, &status)
		c.Status = housechat.Status(status)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("чаты пользователя %d: %w", userID, err)
	}
	return chats, nil
}

// GetActiveForUpdate возвращает участие активного участника активного чата
// и блокирует строку memberships до конца транзакции.
func (r *MembershipRepo) GetActiveForUpdate(ctx context.Context, chatID, userID int64) (membership.Membership, error) {
	return r.getActive(ctx, chatID, userID, " FOR UPDATE OF m")
}

// GetMembership реализует usecase.MemberSettings (без блокировки).
func (r *MembershipRepo) GetMembership(ctx context.Context, chatID, userID int64) (membership.Membership, error) {
	return r.getActive(ctx, chatID, userID, "")
}

func (r *MembershipRepo) getActive(ctx context.Context, chatID, userID int64, lock string) (membership.Membership, error) {
	var m membership.Membership
	var role, status string
	err := querierFrom(ctx, r.pool).QueryRow(ctx, `
		SELECT m.chat_id, m.user_id, m.role, m.rating, m.status, m.forward_to_dm, m.notify_materials
		FROM memberships m
		JOIN house_chats c ON c.id = m.chat_id
		WHERE m.chat_id = $1 AND m.user_id = $2
		  AND m.status = 'active' AND c.status = 'active'`+lock, chatID, userID,
	).Scan(&m.ChatID, &m.UserID, &role, &m.Rating, &status, &m.ForwardToDM, &m.NotifyMaterials)
	if errors.Is(err, pgx.ErrNoRows) {
		return membership.Membership{}, fmt.Errorf("участие (%d, %d): %w", chatID, userID, usecase.ErrNotFound)
	}
	if err != nil {
		return membership.Membership{}, fmt.Errorf("участие (%d, %d): %w", chatID, userID, err)
	}
	m.Role = membership.Role(role)
	m.Status = membership.Status(status)
	return m, nil
}

func (r *MembershipRepo) SetForwardToDM(ctx context.Context, chatID, userID int64, on bool) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx,
		`UPDATE memberships SET forward_to_dm = $3 WHERE chat_id = $1 AND user_id = $2`, chatID, userID, on)
	if err != nil {
		return fmt.Errorf("пересылка в личку (%d, %d): %w", chatID, userID, err)
	}
	return nil
}

func (r *MembershipRepo) SetNotifyMaterials(ctx context.Context, chatID, userID int64, on bool) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx,
		`UPDATE memberships SET notify_materials = $3 WHERE chat_id = $1 AND user_id = $2`, chatID, userID, on)
	if err != nil {
		return fmt.Errorf("уведомления о материалах (%d, %d): %w", chatID, userID, err)
	}
	return nil
}

// ForwardRecipients — активные участники с включённой пересылкой,
// которым бот может писать в личку.
func (r *MembershipRepo) ForwardRecipients(ctx context.Context, chatID, exceptUserID int64) ([]int64, error) {
	rows, err := querierFrom(ctx, r.pool).Query(ctx, `
		SELECT m.user_id
		FROM memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.chat_id = $1 AND m.forward_to_dm AND m.status = 'active'
		  AND m.user_id <> $2 AND NOT u.dm_unavailable
		ORDER BY m.user_id`, chatID, exceptUserID)
	if err != nil {
		return nil, fmt.Errorf("получатели пересылки чата %d: %w", chatID, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("получатели пересылки чата %d: %w", chatID, err)
	}
	return ids, nil
}

// MarkDMUnavailable реализует usecase.DMAvailability.
func (r *MembershipRepo) MarkDMUnavailable(ctx context.Context, userID int64) error {
	_, err := querierFrom(ctx, r.pool).Exec(ctx,
		`UPDATE users SET dm_unavailable = true WHERE id = $1`, userID)
	if err != nil {
		return fmt.Errorf("недоступная личка пользователя %d: %w", userID, err)
	}
	return nil
}
