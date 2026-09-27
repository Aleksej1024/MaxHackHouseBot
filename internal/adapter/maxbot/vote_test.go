package maxbot

import (
	"context"
	"errors"
	"testing"

	maxbotapi "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/domain/vote"
	"maxhouse/internal/usecase"
	"maxhouse/internal/usecase/voting"
)

func chatCallback(payload string) model.Update {
	upd := dmCallback(payload)
	upd.ChatID = -100
	upd.Message.Recipient = model.Recipient{ChatID: -100, ChatType: model.ChatTypeChat}
	return upd
}

func TestRouter_VoteInChat(t *testing.T) {
	f := newRouterFixture()
	f.vote.res = voting.Result{Outcome: voting.OutcomeAccepted, Value: vote.Refute, Type: requesttype.Poll}

	f.r.Process(context.Background(), chatCallback("v:refute:123"))

	assert.Equal(t, []voteCall{{requestID: 123, userID: userID, value: vote.Refute}}, f.vote.calls)
	assert.Equal(t, []answer{{"cb1", "Ваш голос учтён: Нет"}}, f.out.answers)
	assert.Empty(t, f.out.dm, "в личку ничего не пишем")
}

func TestVoteNotification(t *testing.T) {
	tests := []struct {
		name string
		res  voting.Result
		want string
	}{
		{"жалоба За", voting.Result{Outcome: voting.OutcomeAccepted, Value: vote.Confirm, Type: requesttype.Complaint}, "Ваш голос учтён: Подтверждаю"},
		{"инициатива Против", voting.Result{Outcome: voting.OutcomeAccepted, Value: vote.Refute, Type: requesttype.Initiative}, "Ваш голос учтён: Опровергаю"},
		{"опрос Да", voting.Result{Outcome: voting.OutcomeAccepted, Value: vote.Confirm, Type: requesttype.Poll}, "Ваш голос учтён: Да"},
		{"повтор", voting.Result{Outcome: voting.OutcomeUnchanged, Value: vote.Confirm, Type: requesttype.Poll}, "Ваш голос учтён: Да"},
		{"своя заявка", voting.Result{Outcome: voting.OutcomeOwnRequest}, VoteOwnRequest},
		{"завершено", voting.Result{Outcome: voting.OutcomeClosed}, VoteClosed},
		{"не житель", voting.Result{Outcome: voting.OutcomeNotMember}, VoteNotMember},
		{"нет заявки", voting.Result{Outcome: voting.OutcomeNotFound}, VoteNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, voteNotification(tt.res))
		})
	}
}

func TestRouter_VoteBadPayload(t *testing.T) {
	for _, payload := range []string{"v:maybe:1", "v:confirm:abc", "v:confirm", "v", "menu:help", "new:publish"} {
		t.Run(payload, func(t *testing.T) {
			f := newRouterFixture()
			f.r.Process(context.Background(), chatCallback(payload))
			assert.Empty(t, f.vote.calls)
			assert.Empty(t, f.create.calls, "кнопки лички из чата не работают")
			assert.Equal(t, []answer{{"cb1", NotifyUnavailable}}, f.out.answers)
		})
	}
}

func TestRouter_VoteError(t *testing.T) {
	f := newRouterFixture()
	f.vote.err = errors.New("pg down")

	f.r.Process(context.Background(), chatCallback("v:confirm:1"))
	assert.Equal(t, []answer{{"cb1", NotifyError}}, f.out.answers)
}

func TestFillCard_VoteButtonsOnlyWhileOpen(t *testing.T) {
	atts := []request.Attachment{{Kind: request.KindImage, Ref: "t1"}}
	for _, st := range []request.Status{request.StatusOpen, request.StatusInProgress, request.StatusClosed, request.StatusExpired} {
		t.Run(string(st), func(t *testing.T) {
			card := usecase.RequestCard{
				Request:     request.Request{ID: 7, Type: requesttype.Complaint, Status: st, Body: "текст"},
				Attachments: atts,
			}
			body := newCardMessage(card)

			require.NotEmpty(t, body.Attachments)
			assert.Equal(t, model.AttachImage, body.Attachments[0].Type, "медиа сохраняется при правке")
			var payloads []string
			for _, a := range body.Attachments {
				if a.Type == model.AttachInlineKeyboard {
					for _, row := range a.Payload.Buttons {
						for _, b := range row {
							payloads = append(payloads, b.Payload)
						}
					}
				}
			}
			if st == request.StatusOpen {
				assert.Equal(t, []string{"v:confirm:7", "v:refute:7", "open:7"}, payloads)
			} else {
				assert.Equal(t, []string{"open:7"}, payloads, "после голосования — только «Открыть в личке»")
			}
		})
	}
}

func newCardMessage(card usecase.RequestCard) model.NewMessageBody {
	msg := maxbotapi.NewMessage()
	fillCard(msg, card)
	return msg.MessageBody()
}
