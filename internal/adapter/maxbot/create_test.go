package maxbot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/usecase"
	"maxhouse/internal/usecase/onboarding"
	"maxhouse/internal/usecase/requests"
)

var limits = request.CreatePolicy{DailyLimit: 3, MaxBodyLen: 3000, MaxAttachments: 10}

// --- Тексты по типам заявок (раздел 8.0, 15) ---

func TestRequestTypeTexts_EveryRegisteredTypeHasTexts(t *testing.T) {
	for _, tp := range requesttype.Default().All() {
		tt, ok := RequestTypeTexts[tp.Code]
		require.True(t, ok, "нет текстов для типа %q", tp.Code)
		assert.NotEmpty(t, tt.Name, tp.Code)
		assert.NotEmpty(t, tt.Confirm, tp.Code)
		assert.NotEmpty(t, tt.Refute, tp.Code)
		assert.NotEmpty(t, tt.VotedConfirm, tp.Code)
		assert.NotEmpty(t, tt.VotedRefute, tp.Code)
		assert.NotEmpty(t, tt.AudienceAll, tp.Code)
		assert.NotEmpty(t, tt.AudienceConfirm, tp.Code)
		assert.NotEmpty(t, tt.AudienceRefute, tp.Code)
	}
}

func TestVoteKeyboard_LabelsByType(t *testing.T) {
	tests := []struct {
		code            requesttype.Code
		confirm, refute string
	}{
		{requesttype.Complaint, "Подтверждаю", "Опровергаю"},
		{requesttype.Initiative, "Подтверждаю", "Опровергаю"},
		{requesttype.Poll, "Да", "Нет"},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			kb := chatCardKeyboard(request.Request{ID: 123, Type: tt.code, Status: request.StatusOpen})
			labels, payloads := buttons(kb)
			assert.Equal(t, [][]string{{tt.confirm, tt.refute}, {BtnOpenInDM}}, labels)
			assert.Equal(t, []string{"v:confirm:123", "v:refute:123", "open:123"}, payloads,
				"в payload только действие и id, без подписи")
		})
	}
}

// --- Карточка заявки в чате ---

func TestCardText(t *testing.T) {
	req := request.Request{ID: 5, Type: requesttype.Poll, Body: "Ставим шлагбаум?"}

	t.Run("с автором", func(t *testing.T) {
		text := cardText(usecase.RequestCard{Request: req, AuthorNickname: "сосед", Confirms: 2, Refutes: 1})
		assert.Equal(t, "Заявка №5 · Опрос\n\nСтавим шлагбаум?\n\nАвтор: сосед\nДа: 2 · Нет: 1", text)
	})

	t.Run("анонимная", func(t *testing.T) {
		anon := req
		anon.IsAnonymous = true
		text := cardText(usecase.RequestCard{Request: anon, AuthorNickname: "сосед"})
		assert.Contains(t, text, CardAuthorAnonymous)
		assert.NotContains(t, text, "сосед", "ник анонимного автора не попадает в чат")
	})

	t.Run("материалы", func(t *testing.T) {
		text := cardText(usecase.RequestCard{Request: req, Materials: 3})
		assert.True(t, strings.HasSuffix(text, fmt.Sprintf(CardMaterials, 3)))
	})
}

func TestAttachmentsConversion(t *testing.T) {
	in := []model.Attachment{
		{Type: model.AttachImage, Payload: model.Payload{Token: "t1", URL: "https://x"}},
		{Type: model.AttachVideo, Payload: model.Payload{Token: "t2"}},
		{Type: model.AttachFile, Payload: model.Payload{Token: "t3"}},
		{Type: model.AttachAudio, Payload: model.Payload{Token: "t4"}},
		{Type: model.AttachSticker, Payload: model.Payload{Code: "s"}},
		{Type: model.AttachLocation},
		{Type: model.AttachImage}, // без токена
	}
	got := fromMaxAttachments(in)
	assert.Equal(t, []request.Attachment{
		{Kind: request.KindImage, Ref: "t1"},
		{Kind: request.KindVideo, Ref: "t2"},
		{Kind: request.KindFile, Ref: "t3"},
		{Kind: request.KindAudio, Ref: "t4"},
	}, got)

	back := toMaxAttachments(got)
	require.Len(t, back, 4)
	assert.Equal(t, model.AttachImage, back[0].Type)
	assert.Equal(t, "t1", back[0].Payload.Token)
}

// --- Роутинг диалога создания ---

func TestRouter_MenuCreateStartsDialog(t *testing.T) {
	f := newRouterFixture()
	f.create.step = requests.Step{
		Kind:   requests.StepChooseType,
		Types:  requesttype.Default().All(),
		Limits: limits,
	}

	f.r.Process(context.Background(), dmCallback("menu:create"))

	assert.Equal(t, []string{"Begin"}, f.create.calls)
	assert.Equal(t, []answer{{"cb1", NotifyOK}}, f.out.answers)
	require.Len(t, f.out.dm, 1)
	assert.Equal(t, CreateChooseType, f.out.dm[0].text)
	labels, payloads := buttons(f.out.dm[0].kb)
	assert.Equal(t, [][]string{{"Жалоба"}, {"Инициатива"}, {BtnCancel}}, labels)
	assert.Equal(t, []string{"new:type:complaint", "new:type:initiative", "new:cancel"}, payloads)
}

func TestRouter_CreateCallbacksDispatch(t *testing.T) {
	tests := []struct {
		payload  string
		wantCall string
		wantArg  any
	}{
		{"new:type:poll", "ChooseType", requesttype.Poll},
		{"new:done", "FinishMedia", nil},
		{"new:anon:yes", "ChooseAnonymity", true},
		{"new:anon:no", "ChooseAnonymity", false},
		{"new:edit", "Edit", nil},
		{"new:publish", "Publish", membership.User{ID: userID}},
		{"new:cancel", "Cancel", nil},
	}
	for _, tt := range tests {
		t.Run(tt.payload, func(t *testing.T) {
			f := newRouterFixture()
			f.create.step = requests.Step{Kind: requests.StepChooseAnonymity, Limits: limits}

			f.r.Process(context.Background(), dmCallback(tt.payload))

			assert.Equal(t, []string{tt.wantCall}, f.create.calls)
			if tt.wantArg != nil {
				assert.Equal(t, []any{tt.wantArg}, f.create.args)
			}
			assert.Len(t, f.out.answers, 1, "на каждое нажатие есть ответ")
		})
	}
}

func TestRouter_CreateBadCallbacks(t *testing.T) {
	for _, payload := range []string{"new:anon:maybe", "new:unknown", "new"} {
		t.Run(payload, func(t *testing.T) {
			f := newRouterFixture()
			f.r.Process(context.Background(), dmCallback(payload))
			assert.Empty(t, f.create.calls)
			assert.Equal(t, []answer{{"cb1", NotifyUnavailable}}, f.out.answers)
		})
	}
}

func TestRouter_ShowStep(t *testing.T) {
	draft := requests.Draft{Type: requesttype.Complaint, Body: "Курят", Attachments: []request.Attachment{{Kind: request.KindImage, Ref: "t"}}}
	tests := []struct {
		name       string
		step       requests.Step
		wantText   string
		wantLabels [][]string
		toMenu     bool
	}{
		{
			name:       "ввод текста",
			step:       requests.Step{Kind: requests.StepEnterText, Draft: draft},
			wantText:   fmt.Sprintf(CreateEnterText, "Жалоба", 3000),
			wantLabels: [][]string{{BtnCancel}},
		},
		{
			name:       "вложения",
			step:       requests.Step{Kind: requests.StepAddMedia, Draft: draft},
			wantText:   fmt.Sprintf(CreateAddMedia, 1, 10),
			wantLabels: [][]string{{BtnDone}, {BtnCancel}},
		},
		{
			name:       "анонимность",
			step:       requests.Step{Kind: requests.StepChooseAnonymity, Draft: draft},
			wantText:   CreateChooseAnonymity,
			wantLabels: [][]string{{BtnShowAuthor, BtnAnonymous}, {BtnCancel}},
		},
		{
			name:       "подтверждение, анонимно",
			step:       requests.Step{Kind: requests.StepConfirm, Draft: func() requests.Draft { d := draft; d.Anonymous = true; return d }()},
			wantText:   fmt.Sprintf(CreateConfirm, "Жалоба", CreateAuthorAnonymous, 1, "Курят"),
			wantLabels: [][]string{{BtnPublish}, {BtnEditText}, {BtnCancel}},
		},
		{
			name:       "ошибка ввода показывается перед подсказкой",
			step:       requests.Step{Kind: requests.StepEnterText, Draft: draft, Problem: request.ErrBodyTooLong},
			wantText:   fmt.Sprintf(ProblemBodyTooLong, 3000) + "\n\n" + fmt.Sprintf(CreateEnterText, "Жалоба", 3000),
			wantLabels: [][]string{{BtnCancel}},
		},
		{
			name:       "слишком много вложений",
			step:       requests.Step{Kind: requests.StepAddMedia, Draft: draft, Problem: request.ErrTooManyMedia},
			wantText:   fmt.Sprintf(ProblemTooManyMedia, 10) + "\n\n" + fmt.Sprintf(CreateAddMedia, 1, 10),
			wantLabels: [][]string{{BtnDone}, {BtnCancel}},
		},
		{
			name:     "опубликовано",
			step:     requests.Step{Kind: requests.StepPublished, Request: request.Request{ID: 12}, ChatSynced: true},
			wantText: fmt.Sprintf(CreatePublished, 12),
			toMenu:   true,
		},
		{
			name:     "сохранено, чат не синхронизирован",
			step:     requests.Step{Kind: requests.StepPublished, Request: request.Request{ID: 12}},
			wantText: fmt.Sprintf(CreatePublishFailed, 12),
			toMenu:   true,
		},
		{name: "отменено", step: requests.Step{Kind: requests.StepCancelled}, wantText: CreateCancelled, toMenu: true},
		{name: "черновик устарел", step: requests.Step{Kind: requests.StepExpired}, wantText: CreateExpired, toMenu: true},
		{name: "лимит типа при публикации", step: requests.Step{Kind: requests.StepLimitReached, Draft: draft},
			wantText: "За последние сутки вы создали максимальное число заявок типа «Жалоба» (3). Попробуйте позже.", toMenu: true},
		{name: "лимит типа при выборе", step: requests.Step{Kind: requests.StepChooseType, Problem: request.ErrDailyLimit, Types: requesttype.Default().All()},
			wantText:   fmt.Sprintf(ProblemTypeLimit, 3) + "\n\n" + CreateChooseType,
			wantLabels: [][]string{{"Жалоба"}, {"Инициатива"}, {BtnCancel}}},
		{name: "порог рейтинга", step: requests.Step{Kind: requests.StepRatingTooLow}, wantText: CreateRatingTooLow, toMenu: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRouterFixture()
			tt.step.Limits = limits
			f.create.step = tt.step
			f.create.handled = true
			f.dlg.flow = requests.FlowCreate
			f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeReady, Chat: chatA, Chats: nil}

			f.r.Process(context.Background(), dmMessage("текст"))

			require.NotEmpty(t, f.out.dm)
			assert.Equal(t, tt.wantText, f.out.dm[0].text)
			labels, _ := buttons(f.out.dm[0].kb)
			assert.Equal(t, tt.wantLabels, labels)
			if tt.toMenu {
				require.Len(t, f.out.dm, 2, "после итогового шага — меню")
				assert.Equal(t, fmt.Sprintf(DMMenu, "Дом А"), f.out.dm[1].text)
			} else {
				assert.Len(t, f.out.dm, 1)
				assert.Empty(t, f.onb.started)
			}
		})
	}
}

func TestRouter_StepNoChatFallsBackToOnboarding(t *testing.T) {
	f := newRouterFixture()
	f.create.step = requests.Step{Kind: requests.StepNoChat}
	f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeNotRegistered}

	f.r.Process(context.Background(), dmCallback("menu:create"))

	require.Len(t, f.out.dm, 1)
	assert.Equal(t, DMNotRegistered, f.out.dm[0].text)
}

func TestRouter_DialogMessagePassesAttachments(t *testing.T) {
	f := newRouterFixture()
	f.create.handled = true
	f.dlg.flow = requests.FlowCreate
	f.create.step = requests.Step{Kind: requests.StepAddMedia, Limits: limits}
	upd := dmMessage("текст")
	upd.Message.Body.Attachments = []model.Attachment{{Type: model.AttachImage, Payload: model.Payload{Token: "t1"}}}

	f.r.Process(context.Background(), upd)

	assert.Equal(t, []string{"HandleMessage"}, f.create.calls)
	assert.Equal(t, []any{"текст", []request.Attachment{{Kind: request.KindImage, Ref: "t1"}}}, f.create.args)
	assert.Empty(t, f.onb.started, "сообщение ушло в диалог, не в меню")
}

func TestRouter_MessageWithoutDraftGoesToMenu(t *testing.T) {
	f := newRouterFixture()
	f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeNotRegistered}

	f.r.Process(context.Background(), dmMessage("привет"))

	assert.Empty(t, f.create.calls, "без диалога сообщение не идёт в сценарии")
	require.Len(t, f.onb.started, 1)
}

func TestRouter_FSMErrorFallsBackToMenu(t *testing.T) {
	f := newRouterFixture()
	f.dlg.err = errors.New("redis down")
	f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeNotRegistered}

	f.r.Process(context.Background(), dmMessage("привет"))

	require.Len(t, f.onb.started, 1, "при недоступном Redis показываем меню")
}

func TestRouter_CreateErrorAnswersNotify(t *testing.T) {
	f := newRouterFixture()
	f.create.err = errors.New("pg down")

	f.r.Process(context.Background(), dmCallback("new:publish"))

	assert.Equal(t, []answer{{"cb1", NotifyError}}, f.out.answers)
	assert.Empty(t, f.out.dm)
}
