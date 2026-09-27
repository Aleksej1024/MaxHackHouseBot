// Package settings — «Настройки» жителя в активном домовом чате:
// новые заявки дома в личку (раздел 8.6), уведомления о комментариях к своим
// заявкам (раздел 8.4) и статус создания заявок (раздел 9: рейтинг
// внутренний, показываем только статус).
package settings

import (
	"context"
	"errors"
	"fmt"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/usecase"
)

// ActiveChats — активный чат пользователя (onboarding.Service).
type ActiveChats interface {
	ActiveChat(ctx context.Context, userID int64) (housechat.Chat, bool, error)
}

// Result — экран настроек.
type Result struct {
	// NoChat — активного чата нет: транспорт запускает сценарий первого сообщения.
	NoChat          bool
	Chat            housechat.Chat
	ForwardToDM     bool
	NotifyMaterials bool
	// CanCreate — рейтинг не ниже RATING_MIN_TO_CREATE. Число рейтинга
	// пользователю не показывается.
	CanCreate bool
}

type Service struct {
	chats       ActiveChats
	members     usecase.MemberSettings
	minToCreate int
}

func New(chats ActiveChats, members usecase.MemberSettings, minToCreate int) *Service {
	return &Service{chats: chats, members: members, minToCreate: minToCreate}
}

// Get — текущие настройки в активном чате.
func (s *Service) Get(ctx context.Context, userID int64) (Result, error) {
	chat, ok, err := s.chats.ActiveChat(ctx, userID)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{NoChat: true}, nil
	}
	m, err := s.members.GetMembership(ctx, chat.ID, userID)
	if errors.Is(err, usecase.ErrNotFound) {
		return Result{NoChat: true}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("настройки участника: %w", err)
	}
	return Result{
		Chat:            chat,
		ForwardToDM:     m.ForwardToDM,
		NotifyMaterials: m.NotifyMaterials,
		CanCreate:       m.Rating >= s.minToCreate,
	}, nil
}

// ToggleNotifyMaterials включает или выключает уведомления о комментариях
// и доказательствах к своим заявкам.
func (s *Service) ToggleNotifyMaterials(ctx context.Context, userID int64) (Result, error) {
	res, err := s.Get(ctx, userID)
	if err != nil || res.NoChat {
		return res, err
	}
	if err := s.members.SetNotifyMaterials(ctx, res.Chat.ID, userID, !res.NotifyMaterials); err != nil {
		return Result{}, fmt.Errorf("уведомления о комментариях: %w", err)
	}
	res.NotifyMaterials = !res.NotifyMaterials
	return res, nil
}

// ToggleForward включает или выключает новые заявки дома в личку.
func (s *Service) ToggleForward(ctx context.Context, userID int64) (Result, error) {
	res, err := s.Get(ctx, userID)
	if err != nil || res.NoChat {
		return res, err
	}
	if err := s.members.SetForwardToDM(ctx, res.Chat.ID, userID, !res.ForwardToDM); err != nil {
		return Result{}, fmt.Errorf("пересылка в личку: %w", err)
	}
	res.ForwardToDM = !res.ForwardToDM
	return res, nil
}
