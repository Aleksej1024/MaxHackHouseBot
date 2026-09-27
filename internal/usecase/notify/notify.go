// Package notify — сообщения жителям в личку: пересылка новых заявок
// (раздел 8.6), уведомления автору о материалах (раздел 8.4) и доставка
// рассылок (раздел 8.5). Бот пишет в личку только тем, кто начал с ним
// диалог; недоставка таким пользователям — не ошибка.
package notify

import (
	"context"
	"errors"
	"log/slog"

	"maxhouse/internal/domain/comment"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/usecase"
)

// Notifier отправляет сообщения в личку.
type Notifier struct {
	cards    usecase.CardReader
	settings usecase.MemberSettings
	dm       usecase.DMAvailability
	out      usecase.UserNotifier
	log      *slog.Logger
}

func New(cards usecase.CardReader, settings usecase.MemberSettings, dm usecase.DMAvailability,
	out usecase.UserNotifier, log *slog.Logger) *Notifier {
	return &Notifier{cards: cards, settings: settings, dm: dm, out: out, log: log}
}

// Delivery — итог доставки.
type Delivery struct {
	Delivered   int
	Unavailable int // не начинали диалог или заблокировали бота
	Failed      int // временные ошибки после всех повторов
}

// Deliver отправляет сообщение каждому получателю. Пользователи, которым
// писать нельзя, помечаются недоступными и больше не выбираются.
func (n *Notifier) Deliver(ctx context.Context, userIDs []int64, send func(ctx context.Context, userID int64) error) Delivery {
	var d Delivery
	for _, uid := range userIDs {
		if ctx.Err() != nil {
			d.Failed += len(userIDs) - d.Delivered - d.Unavailable - d.Failed
			break
		}
		err := send(ctx, uid)
		switch {
		case err == nil:
			d.Delivered++
		case errors.Is(err, usecase.ErrDMUnavailable):
			d.Unavailable++
			if err := n.dm.MarkDMUnavailable(ctx, uid); err != nil {
				n.log.Error("отметка недоступной лички", "user_id", uid, "error", err)
			}
		default:
			d.Failed++
			n.log.Error("сообщение в личку не доставлено", "user_id", uid, "error", err)
		}
	}
	return d
}

// ForwardNew дублирует новую заявку в личку жителям чата, включившим
// пересылку (кроме автора).
func (n *Notifier) ForwardNew(ctx context.Context, requestID int64) Delivery {
	card, err := n.cards.LoadCard(ctx, requestID)
	if err != nil {
		n.log.Error("пересылка заявки: карточка", "request_id", requestID, "error", err)
		return Delivery{}
	}
	ids, err := n.settings.ForwardRecipients(ctx, card.Request.ChatID, card.Request.AuthorID)
	if err != nil {
		n.log.Error("пересылка заявки: получатели", "request_id", requestID, "error", err)
		return Delivery{}
	}
	d := n.Deliver(ctx, ids, func(ctx context.Context, uid int64) error {
		return n.out.ForwardRequest(ctx, uid, card)
	})
	if len(ids) > 0 {
		n.log.Info("заявка переслана в личку", "request_id", requestID,
			"delivered", d.Delivered, "unavailable", d.Unavailable, "failed", d.Failed)
	}
	return d
}

// MaterialAdded уведомляет автора заявки о новом материале, если у автора
// включены уведомления о комментариях (по умолчанию включены). Возвращает
// true, если уведомление доставлено.
func (n *Notifier) MaterialAdded(ctx context.Context, req request.Request, c comment.Comment) bool {
	m, err := n.settings.GetMembership(ctx, req.ChatID, req.AuthorID)
	if err != nil {
		if !errors.Is(err, usecase.ErrNotFound) {
			n.log.Error("уведомление о материале: настройки автора", "request_id", req.ID, "error", err)
		}
		return false
	}
	if !m.NotifyMaterials {
		return false
	}
	d := n.Deliver(ctx, []int64{req.AuthorID}, func(ctx context.Context, uid int64) error {
		return n.out.NotifyMaterial(ctx, uid, req, c)
	})
	return d.Delivered == 1
}
