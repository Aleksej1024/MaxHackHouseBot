package voting

import (
	"context"
	"fmt"
	"log/slog"

	"maxhouse/internal/domain/rating"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/usecase"
)

// Rater начисляет рейтинг автору по итогам голосования (раздел 9).
type Rater struct {
	requests usecase.RequestLocker
	votes    usecase.VoteRepo
	ratings  usecase.RatingRepo
	policy   rating.Policy
	log      *slog.Logger
}

func NewRater(requests usecase.RequestLocker, votes usecase.VoteRepo, ratings usecase.RatingRepo, policy rating.Policy, log *slog.Logger) *Rater {
	return &Rater{requests: requests, votes: votes, ratings: ratings, policy: policy, log: log}
}

// Apply считает рейтинг по завершённому голосованию. Вызывается один раз
// в момент завершения голосования: переход open → in_progress, досрочное
// закрытие автором или истечение срока — что наступит раньше.
//
// Должен выполняться внутри транзакции вызывающего, который уже заблокировал
// заявку (RequestLocker.GetForUpdate). Идемпотентен: повторный вызов
// не меняет рейтинг (флаг rating_applied). По удалённой заявке рейтинг
// не начисляется и не снимается (раздел 8.7), флаг при этом не ставится.
// Возвращает применённое изменение (0 — рейтинг не менялся).
func (r *Rater) Apply(ctx context.Context, req request.Request) (int, error) {
	if req.Status == request.StatusDeleted {
		return 0, nil
	}
	claimed, err := r.requests.ClaimRating(ctx, req.ID)
	if err != nil {
		return 0, fmt.Errorf("флаг rating_applied заявки %d: %w", req.ID, err)
	}
	if !claimed {
		return 0, nil
	}

	confirms, refutes, err := r.votes.Count(ctx, req.ID)
	if err != nil {
		return 0, fmt.Errorf("подсчёт голосов заявки %d: %w", req.ID, err)
	}
	delta, reason, ok := r.policy.Delta(req.Type, confirms, refutes)
	if !ok {
		return 0, nil
	}
	err = r.ratings.AddEvent(ctx, rating.Event{
		ChatID:    req.ChatID,
		UserID:    req.AuthorID,
		RequestID: req.ID,
		Delta:     delta,
		Reason:    reason,
	})
	if err != nil {
		return 0, fmt.Errorf("событие рейтинга заявки %d: %w", req.ID, err)
	}
	r.log.Info("рейтинг изменён",
		"request_id", req.ID, "chat_id", req.ChatID, "user_id", req.AuthorID,
		"delta", delta, "reason", string(reason))
	return delta, nil
}
