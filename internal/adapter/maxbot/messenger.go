package maxbot

import (
	"context"

	maxbotapi "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

// Messenger реализует usecase.Messenger через MAX API с повторами.
type Messenger struct {
	api *maxbotapi.Api
	rt  *retrier
}

func NewMessenger(api *maxbotapi.Api, opts RetryOptions) *Messenger {
	return &Messenger{api: api, rt: newRetrier(opts)}
}

// SendChatMessage отправляет сообщение в чат и возвращает его id (mid) в MAX.
func (m *Messenger) SendChatMessage(ctx context.Context, chatID int64, text string) (string, error) {
	var messageID string
	err := m.rt.do(ctx, func(ctx context.Context) error {
		res, err := m.api.Messages.Send(ctx, maxbotapi.NewMessage().SetChat(chatID).SetText(text))
		if err != nil {
			return err
		}
		messageID = res.Message.Body.Mid
		return nil
	})
	if err != nil {
		return "", err
	}
	return messageID, nil
}

// SendUserMessage отправляет сообщение в личный диалог с пользователем.
// kb может быть nil.
func (m *Messenger) SendUserMessage(ctx context.Context, userID int64, text string, kb *model.Keyboard) error {
	return m.rt.do(ctx, func(ctx context.Context) error {
		msg := maxbotapi.NewMessage().SetUser(userID).SetText(text)
		if kb != nil {
			msg.AddKeyboard(kb)
		}
		_, err := m.api.Messages.Send(ctx, msg)
		return err
	})
}

// AnswerCallback отвечает на нажатие кнопки коротким уведомлением.
func (m *Messenger) AnswerCallback(ctx context.Context, callbackID, notification string) error {
	return m.rt.do(ctx, func(ctx context.Context) error {
		_, err := m.api.Messages.AnswerOnCallback(ctx, callbackID, model.CallbackAnswer{Notification: &notification})
		return err
	})
}
