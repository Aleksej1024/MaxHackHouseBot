// Package voting — голосование по заявкам и рейтинг.
package voting

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/domain/vote"
	"maxhouse/internal/usecase"
)

// Outcome — итог нажатия кнопки голосования.
type Outcome int

const (
	// OutcomeAccepted — голос учтён (новый или изменённый).
	OutcomeAccepted Outcome = iota + 1
	// OutcomeUnchanged — повторное нажатие той же кнопки, голос уже учтён.
	OutcomeUnchanged
	// OutcomeOwnRequest — автор не голосует за свою заявку.
	OutcomeOwnRequest
	// OutcomeClosed — голосование завершено, голоса зафиксированы.
	OutcomeClosed
	// OutcomeNotMember — голосуют только активные участники этого чата.
	OutcomeNotMember
	// OutcomeNotFound — заявки нет или она удалена.
	OutcomeNotFound
)

// Result — что ответить на нажатие.
type Result struct {
	Outcome Outcome
	Value   vote.Value
	Type    requesttype.Code
}

// ChatSyncer обновляет карточку заявки в чате (реализует chatsync.Syncer).
type ChatSyncer interface {
	Sync(ctx context.Context, requestID int64) bool
}

// Deps — зависимости сценария голосования.
type Deps struct {
	Requests usecase.RequestLocker
	Members  usecase.MembershipReader
	Votes    usecase.VoteRepo
	Sync     ChatSyncer
	Tx       usecase.TxManager
	Clock    usecase.Clock
	Log      *slog.Logger
}

// Service реализует голосование.
type Service struct {
	Deps
}

func New(deps Deps) *Service {
	return &Service{Deps: deps}
}

// Vote обрабатывает нажатие «За»/«Против». Данным кнопки не доверяем:
// заявка, статус, участие и авторство проверяются по БД. Повторное
// нажатие (или повторный апдейт) той же кнопки ничего не меняет.
func (s *Service) Vote(ctx context.Context, requestID, userID int64, value vote.Value) (Result, error) {
	var res Result
	err := s.Tx.Do(ctx, func(ctx context.Context) error {
		req, err := s.Requests.GetForUpdate(ctx, requestID)
		if errors.Is(err, usecase.ErrNotFound) {
			res.Outcome = OutcomeNotFound
			return nil
		}
		if err != nil {
			return fmt.Errorf("заявка %d: %w", requestID, err)
		}
		res.Type, res.Value = req.Type, value

		switch {
		case req.Status == request.StatusDeleted:
			res.Outcome = OutcomeNotFound
			return nil
		case req.AuthorID == userID:
			res.Outcome = OutcomeOwnRequest
			return nil
		case !req.VotingOpen(s.Clock.Now()):
			res.Outcome = OutcomeClosed
			return nil
		}

		if _, err := s.Members.GetActiveForUpdate(ctx, req.ChatID, userID); err != nil {
			if errors.Is(err, usecase.ErrNotFound) {
				res.Outcome = OutcomeNotMember
				return nil
			}
			return fmt.Errorf("участие пользователя: %w", err)
		}

		prev, err := s.Votes.Upsert(ctx, requestID, userID, value)
		if err != nil {
			return fmt.Errorf("голос: %w", err)
		}
		res.Outcome = OutcomeAccepted
		if prev == value {
			res.Outcome = OutcomeUnchanged
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}

	if res.Outcome == OutcomeAccepted {
		s.Log.Debug("голос учтён", "request_id", requestID, "user_id", userID, "value", string(value))
		s.Sync.Sync(ctx, requestID)
	}
	return res, nil
}
