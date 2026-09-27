// Package registration — сценарий регистрации жителей:
// добавление и удаление бота в чате, отслеживание входа и выхода участников.
// Чат в статусе pending организатор активирует прямо в БД (см. README).
//
// Use case ничего не знает о MAX и не содержит текстов сообщений:
// текст приветствия выбирает транспорт по возвращённому статусу чата.
package registration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/usecase"
)

// UserSessions — временное состояние пользователя в личке (Redis):
// выбранный дом и незавершённый диалог.
type UserSessions interface {
	// LeftChat вызывается после выхода из чата: если этот дом выбран в сессии,
	// сессия переключается на оставшийся дом или сбрасывается целиком,
	// а незавершённый диалог прерывается.
	LeftChat(ctx context.Context, userID, chatID int64) error
}

// Service реализует сценарий регистрации.
type Service struct {
	chats        usecase.ChatRepo
	memberships  usecase.MembershipRepo
	members      usecase.MemberLister
	sessions     UserSessions
	tx           usecase.TxManager
	autoActivate bool
	log          *slog.Logger
}

func New(
	chats usecase.ChatRepo,
	memberships usecase.MembershipRepo,
	members usecase.MemberLister,
	sessions UserSessions,
	tx usecase.TxManager,
	autoActivate bool,
	log *slog.Logger,
) *Service {
	return &Service{
		chats:        chats,
		memberships:  memberships,
		members:      members,
		sessions:     sessions,
		tx:           tx,
		autoActivate: autoActivate,
		log:          log,
	}
}

// BotAdded обрабатывает добавление бота в чат:
//  1. В транзакции создаёт чат (active при autoActivate, иначе pending)
//     или обновляет его title; чат со статусом bot_removed реактивируется.
//  2. Вне транзакции запрашивает у MAX список участников и сохраняет их.
//
// Если title пуст (в событии bot_added MAX его не присылает), название
// запрашивается у MAX; ошибка не мешает регистрации.
//
// Возвращает итоговый статус чата, чтобы транспорт выбрал текст сообщения.
func (s *Service) BotAdded(ctx context.Context, chatID int64, title string) (housechat.Status, error) {
	if title == "" {
		t, err := s.members.ChatTitle(ctx, chatID)
		if err != nil {
			s.log.Warn("название чата не получено", "chat_id", chatID, "error", err)
		}
		title = t
	}
	def := housechat.StatusPending
	if s.autoActivate {
		def = housechat.StatusActive
	}

	var status housechat.Status
	err := s.tx.Do(ctx, func(ctx context.Context) error {
		if err := s.chats.Upsert(ctx, housechat.Chat{ID: chatID, Title: title, Status: def}); err != nil {
			return fmt.Errorf("upsert чата: %w", err)
		}
		chat, err := s.chats.Get(ctx, chatID)
		if err != nil {
			return fmt.Errorf("получение чата: %w", err)
		}
		status = chat.Status
		if status == housechat.StatusBotRemoved {
			if err := s.chats.SetStatus(ctx, chatID, def); err != nil {
				return fmt.Errorf("реактивация чата: %w", err)
			}
			status = def
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	members, err := s.members.ListMembers(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("список участников чата: %w", err)
	}
	for _, m := range members {
		if m.IsBot {
			continue
		}
		if err := s.memberships.UpsertUser(ctx, membership.User{ID: m.UserID, Nickname: m.Nickname}); err != nil {
			return "", fmt.Errorf("upsert пользователя %d: %w", m.UserID, err)
		}
		role := membership.RoleMember
		if m.IsAdmin {
			role = membership.RoleAdmin
		}
		err := s.memberships.UpsertMembership(ctx, membership.Membership{
			ChatID: chatID,
			UserID: m.UserID,
			Role:   role,
			Status: membership.StatusActive,
		})
		if err != nil {
			return "", fmt.Errorf("upsert участия пользователя %d: %w", m.UserID, err)
		}
	}

	s.log.Info("бот добавлен в чат",
		"chat_id", chatID,
		"status", string(status),
		"members", len(members),
	)
	return status, nil
}

// ChatTitleChanged обновляет название известного чата; статус не меняется.
// Неизвестный чат пропускается.
func (s *Service) ChatTitleChanged(ctx context.Context, chatID int64, title string) error {
	chat, err := s.chats.Get(ctx, chatID)
	if errors.Is(err, usecase.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("получение чата: %w", err)
	}
	chat.Title = title
	if err := s.chats.Upsert(ctx, chat); err != nil {
		return fmt.Errorf("название чата: %w", err)
	}
	s.log.Debug("название чата изменено", "chat_id", chatID)
	return nil
}

// BotRemoved обрабатывает удаление бота из чата. Отсутствие чата — не ошибка.
func (s *Service) BotRemoved(ctx context.Context, chatID int64) error {
	if err := s.chats.SetStatus(ctx, chatID, housechat.StatusBotRemoved); err != nil {
		return fmt.Errorf("статус чата: %w", err)
	}
	s.log.Info("бот удалён из чата", "chat_id", chatID)
	return nil
}

// UserAdded обрабатывает вход пользователя в чат. Если чата нет в БД,
// событие пропускается (участие ссылается на чат внешним ключом).
func (s *Service) UserAdded(ctx context.Context, chatID int64, u membership.User) error {
	if _, err := s.chats.Get(ctx, chatID); err != nil {
		if errors.Is(err, usecase.ErrNotFound) {
			s.log.Debug("вход участника в неизвестный чат, пропуск",
				"chat_id", chatID, "user_id", u.ID)
			return nil
		}
		return fmt.Errorf("получение чата: %w", err)
	}
	if err := s.memberships.UpsertUser(ctx, u); err != nil {
		return fmt.Errorf("upsert пользователя %d: %w", u.ID, err)
	}
	err := s.memberships.UpsertMembership(ctx, membership.Membership{
		ChatID: chatID,
		UserID: u.ID,
		Role:   membership.RoleMember,
		Status: membership.StatusActive,
	})
	if err != nil {
		return fmt.Errorf("upsert участия пользователя %d: %w", u.ID, err)
	}
	s.log.Debug("участник вошёл в чат", "chat_id", chatID, "user_id", u.ID)
	return nil
}

// UserRemoved обрабатывает выход пользователя из чата.
// Отсутствие записи об участии — не ошибка.
//
// После статуса left обновляется сессия в личке: если ушедший дом был
// выбран, активным становится оставшийся дом, а если домов не осталось —
// сессия и незавершённый диалог сбрасываются целиком. Ошибка Redis выходу
// не мешает (предупреждение в логе).
func (s *Service) UserRemoved(ctx context.Context, chatID, userID int64) error {
	if err := s.memberships.SetStatus(ctx, chatID, userID, membership.StatusLeft); err != nil {
		return fmt.Errorf("статус участия пользователя %d: %w", userID, err)
	}
	if err := s.sessions.LeftChat(ctx, userID, chatID); err != nil {
		s.log.Warn("сессия вышедшего участника", "chat_id", chatID, "user_id", userID, "error", err)
	}
	s.log.Debug("участник вышел из чата", "chat_id", chatID, "user_id", userID)
	return nil
}
