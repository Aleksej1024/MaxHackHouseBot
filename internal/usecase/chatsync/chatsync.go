// Package chatsync приводит сообщение заявки в домовом чате к состоянию БД
// Вызывается после коммита
// транзакции: данные в БД из-за ошибки MAX не откатываются.
package chatsync

import (
	"context"
	"log/slog"

	"maxhouse/internal/domain/request"
	"maxhouse/internal/usecase"
)

// Syncer синхронизирует карточку заявки с чатом.
type Syncer struct {
	cards usecase.CardReader
	pub   usecase.ChatPublisher
	state usecase.ChatSyncState
	log   *slog.Logger
}

func New(cards usecase.CardReader, pub usecase.ChatPublisher, state usecase.ChatSyncState, log *slog.Logger) *Syncer {
	return &Syncer{cards: cards, pub: pub, state: state, log: log}
}

// Sync обновляет карточку заявки в чате. Если сообщения ещё нет
// (прошлая публикация не удалась), публикует его заново. Повторы временных
// ошибок выполняет адаптер; после неудачи ставится chat_sync = failed,
// следующее изменение заявки попробует снова. Возвращает true при успехе.
func (s *Syncer) Sync(ctx context.Context, requestID int64) bool {
	attrs := []any{"request_id", requestID}
	card, err := s.cards.LoadCard(ctx, requestID)
	if err != nil {
		s.log.Error("загрузка карточки заявки", append(attrs, "error", err)...)
		return false
	}
	attrs = append(attrs, "chat_id", card.Request.ChatID)

	// Удалённая заявка: сообщение в чате удаляется (раздел 8.7).
	if card.Request.Status == request.StatusDeleted {
		if card.Request.ChatMessageID == "" {
			return true
		}
		if err := s.pub.DeleteRequest(ctx, card.Request.ChatMessageID); err != nil {
			s.fail(ctx, requestID, err, attrs)
			return false
		}
		if err := s.state.SetChatMessage(ctx, requestID, card.Request.ChatMessageID); err != nil {
			s.log.Error("отметка chat_sync = ok", append(attrs, "error", err)...)
		}
		return true
	}

	if card.Request.ChatMessageID == "" {
		mid, err := s.pub.PublishRequest(ctx, card)
		if err != nil {
			s.fail(ctx, requestID, err, attrs)
			return false
		}
		if err := s.state.SetChatMessage(ctx, requestID, mid); err != nil {
			s.log.Error("сохранение chat_message_id", append(attrs, "error", err)...)
		}
		return true
	}

	if err := s.pub.EditRequest(ctx, card); err != nil {
		s.fail(ctx, requestID, err, attrs)
		return false
	}
	if err := s.state.SetChatMessage(ctx, requestID, card.Request.ChatMessageID); err != nil {
		s.log.Error("отметка chat_sync = ok", append(attrs, "error", err)...)
	}
	return true
}

// PublishResult публикует в чате итог закрытой заявки (раздел 8.7).
// Ошибка публикации итога не помечает chat_sync: карточка уже обновлена,
// итог виден в ней самой.
func (s *Syncer) PublishResult(ctx context.Context, requestID int64) bool {
	attrs := []any{"request_id", requestID}
	card, err := s.cards.LoadCard(ctx, requestID)
	if err != nil {
		s.log.Error("загрузка карточки заявки", append(attrs, "error", err)...)
		return false
	}
	if card.Request.Status != request.StatusClosed {
		return false
	}
	if err := s.pub.PublishResult(ctx, card); err != nil {
		s.log.Error("публикация итога заявки", append(attrs, "chat_id", card.Request.ChatID, "error", err)...)
		return false
	}
	return true
}

func (s *Syncer) fail(ctx context.Context, requestID int64, cause error, attrs []any) {
	s.log.Error("синхронизация заявки с чатом", append(attrs, "error", cause)...)
	if err := s.state.SetChatSyncFailed(ctx, requestID); err != nil {
		s.log.Error("отметка chat_sync = failed", append(attrs, "error", err)...)
	}
}
