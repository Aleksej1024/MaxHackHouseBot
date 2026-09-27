package maxbot

import (
	"context"
	"fmt"
	"strings"

	maxbotapi "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"

	"maxhouse/internal/domain/request"
	"maxhouse/internal/usecase"
)

// PublishRequest реализует usecase.ChatPublisher: отправляет карточку
// заявки с вложениями и кнопками голосования в домовой чат.
func (m *Messenger) PublishRequest(ctx context.Context, card usecase.RequestCard) (string, error) {
	var messageID string
	err := m.rt.do(ctx, func(ctx context.Context) error {
		msg := maxbotapi.NewMessage().SetChat(card.Request.ChatID)
		fillCard(msg, card)
		res, err := m.api.Messages.Send(ctx, msg)
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

// EditRequest реализует usecase.ChatPublisher: заменяет текст, вложения
// и кнопки опубликованной карточки. Вложения передаются заново: пустой
// список в MAX удалил бы медиа, а клавиатура — тоже вложение.
func (m *Messenger) EditRequest(ctx context.Context, card usecase.RequestCard) error {
	return m.rt.do(ctx, func(ctx context.Context) error {
		msg := maxbotapi.NewMessage()
		fillCard(msg, card)
		_, err := m.api.Messages.EditMessage(ctx, card.Request.ChatMessageID, msg.MessageBody())
		return err
	})
}

// fillCard заполняет сообщение карточкой: текст, медиа и кнопки. Кнопки
// голосования — только пока оно идёт (раздел 8.3), «Открыть в личке» — всегда.
func fillCard(msg *maxbotapi.Message, card usecase.RequestCard) {
	// AddAttachments заменяет список вложений, поэтому клавиатура — после.
	msg.SetText(cardText(card)).AddAttachments(toMaxAttachments(card.Attachments))
	msg.AddKeyboard(chatCardKeyboard(card.Request))
}

// cardText — текст карточки заявки в чате. Автор анонимной заявки
// не показывается: ник в карточку вообще не передаётся.
func cardText(card usecase.RequestCard) string {
	tt := typeTexts(card.Request.Type)
	var b strings.Builder
	fmt.Fprintf(&b, CardHeader, card.Request.ID, tt.Name)
	b.WriteString("\n\n")
	b.WriteString(card.Request.Body)
	b.WriteString("\n\n")
	if card.Request.IsAnonymous || card.AuthorNickname == "" {
		b.WriteString(CardAuthorAnonymous)
	} else {
		fmt.Fprintf(&b, CardAuthor, card.AuthorNickname)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, CardVotes, tt.Confirm, card.Confirms, tt.Refute, card.Refutes)
	if card.Materials > 0 {
		b.WriteString("\n")
		fmt.Fprintf(&b, CardMaterials, card.Materials)
	}
	if status := cardStatus(card.Request); status != "" {
		b.WriteString("\n\n")
		b.WriteString(status)
	}
	return b.String()
}

// cardStatus — строка статуса для карточки в чате (пусто, пока идёт голосование).
func cardStatus(r request.Request) string {
	switch r.Status {
	case request.StatusInProgress:
		return CardStatusInProgress
	case request.StatusClosed:
		return fmt.Sprintf(CardStatusClosed, r.ResultText)
	case request.StatusExpired:
		return CardStatusExpired
	}
	return ""
}

// DeleteRequest реализует usecase.ChatPublisher: удаляет сообщение заявки.
func (m *Messenger) DeleteRequest(ctx context.Context, messageID string) error {
	return m.rt.do(ctx, func(ctx context.Context) error {
		_, err := m.api.Messages.DeleteMessage(ctx, messageID)
		return err
	})
}

// PublishResult реализует usecase.ChatPublisher: итог закрытой заявки
// ответом на её карточку (или отдельным сообщением, если карточки нет).
func (m *Messenger) PublishResult(ctx context.Context, card usecase.RequestCard) error {
	text := fmt.Sprintf(ResultPosted, card.Request.ID, card.Request.ResultText)
	return m.rt.do(ctx, func(ctx context.Context) error {
		msg := maxbotapi.NewMessage().SetChat(card.Request.ChatID)
		if card.Request.ChatMessageID != "" {
			msg.SetReply(text, card.Request.ChatMessageID)
		} else {
			msg.SetText(text)
		}
		_, err := m.api.Messages.Send(ctx, msg)
		return err
	})
}

// toMaxAttachments восстанавливает вложения MAX по сохранённым токенам.
func toMaxAttachments(atts []request.Attachment) []model.Attachment {
	out := make([]model.Attachment, 0, len(atts))
	for _, a := range atts {
		out = append(out, model.Attachment{
			Type:    model.AttachmentType(a.Kind),
			Payload: model.Payload{Token: a.Ref},
		})
	}
	return out
}

// fromMaxAttachments извлекает из входящего сообщения поддерживаемые
// вложения (фото, видео, файл, аудио). Остальные (стикеры, контакты,
// геолокация, клавиатуры) и вложения без токена пропускаются.
func fromMaxAttachments(atts []model.Attachment) []request.Attachment {
	var out []request.Attachment
	for _, a := range atts {
		var kind request.AttachmentKind
		switch a.Type {
		case model.AttachImage:
			kind = request.KindImage
		case model.AttachVideo:
			kind = request.KindVideo
		case model.AttachFile:
			kind = request.KindFile
		case model.AttachAudio:
			kind = request.KindAudio
		default:
			continue
		}
		if a.Payload.Token == "" {
			continue
		}
		out = append(out, request.Attachment{Kind: kind, Ref: a.Payload.Token})
	}
	return out
}
