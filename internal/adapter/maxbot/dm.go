package maxbot

import (
	"context"
	"errors"
	"fmt"

	maxbotapi "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"

	"maxhouse/internal/domain/comment"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/usecase"
)

// SendUserMedia отправляет в личку сообщение с вложениями MAX (по токенам)
// и необязательной клавиатурой.
func (m *Messenger) SendUserMedia(ctx context.Context, userID int64, text string, atts []request.Attachment, kb *model.Keyboard) error {
	return m.rt.do(ctx, func(ctx context.Context) error {
		msg := maxbotapi.NewMessage().SetUser(userID).SetText(text).AddAttachments(toMaxAttachments(atts))
		if kb != nil {
			msg.AddKeyboard(kb)
		}
		_, err := m.api.Messages.Send(ctx, msg)
		return err
	})
}

// SendBroadcast реализует usecase.UserNotifier: автор заявки не раскрывается.
func (m *Messenger) SendBroadcast(ctx context.Context, userID int64, req request.Request, text string) error {
	body := fmt.Sprintf(BroadcastMessage, req.ID, typeTexts(req.Type).Name, text)
	return dmError(m.SendUserMessage(ctx, userID, body, openKeyboard(openCallback(req.ID))))
}

// NotifyMaterial реализует usecase.UserNotifier: автору — новый материал
// с ником отправителя и вложениями.
func (m *Messenger) NotifyMaterial(ctx context.Context, authorID int64, req request.Request, c comment.Comment) error {
	body := fmt.Sprintf(MaterialNotification, MaterialKindNames[c.Kind], req.ID, c.AuthorNickname, c.Body)
	return dmError(m.SendUserMedia(ctx, authorID, body, c.Attachments, openKeyboard(reqCallback(reqOpen, req.ID))))
}

// ForwardRequest реализует usecase.UserNotifier: новая заявка в личку.
func (m *Messenger) ForwardRequest(ctx context.Context, userID int64, card usecase.RequestCard) error {
	body := fmt.Sprintf(ForwardedRequest, cardText(card))
	return dmError(m.SendUserMedia(ctx, userID, body, card.Attachments, openKeyboard(openCallback(card.Request.ID))))
}

// dmError переводит постоянную ошибку MAX (диалог не начат, бот заблокирован)
// в usecase.ErrDMUnavailable. Временные ошибки уже повторены retrier.
func dmError(err error) error {
	if err == nil || isTemporary(err) {
		return err
	}
	var apiErr *maxbotapi.Error
	if errors.As(err, &apiErr) {
		return fmt.Errorf("%w: %s", usecase.ErrDMUnavailable, apiErr.Code)
	}
	return err
}
