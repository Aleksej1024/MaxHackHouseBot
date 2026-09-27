package maxbot

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/usecase"
	"maxhouse/internal/usecase/onboarding"
	"maxhouse/internal/usecase/requests"
)

func TestRouter_MyRequestsList(t *testing.T) {
	f := newRouterFixture()
	f.manage.res = requests.ManageResult{
		Kind: requests.ManageList,
		Chat: housechat.Chat{ID: 100, Title: "Дом А"},
		Requests: []request.Request{
			{ID: 12, Type: requesttype.Poll, Status: request.StatusOpen},
			{ID: 9, Type: requesttype.Complaint, Status: request.StatusClosed},
		},
	}

	f.r.Process(context.Background(), dmCallback("menu:mine"))

	assert.Equal(t, []string{"MyRequests"}, f.manage.calls)
	require.Len(t, f.out.dm, 1)
	assert.Equal(t, fmt.Sprintf(MyRequestsTitle, "Дом А"), f.out.dm[0].text)
	labels, payloads := buttons(f.out.dm[0].kb)
	assert.Equal(t, [][]string{{"№12 · Опрос · идёт голосование"}, {"№9 · Жалоба · закрыта"}, {BtnToMenu}}, labels)
	assert.Equal(t, []string{"req:open:12", "req:open:9", "menu:main"}, payloads)
}

func TestRouter_MyRequestsEmptyShowsMenu(t *testing.T) {
	f := newRouterFixture()
	f.manage.res = requests.ManageResult{Kind: requests.ManageList, Chat: chatA}
	f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeReady, Chat: chatA}

	f.r.Process(context.Background(), dmCallback("menu:mine"))

	require.Len(t, f.out.dm, 2)
	assert.Equal(t, fmt.Sprintf(MyRequestsEmpty, "Дом А"), f.out.dm[0].text)
	assert.Equal(t, fmt.Sprintf(DMMenu, "Дом А"), f.out.dm[1].text)
}

func TestRouter_ManageCallbacksDispatch(t *testing.T) {
	tests := []struct {
		payload, call string
		id            int64
	}{
		{"req:open:5", "Open", 5},
		{"req:edit:5", "StartEdit", 5},
		{"req:close:5", "StartClose", 5},
		{"req:delete:5", "AskDelete", 5},
		{"req:delete_yes:5", "Delete", 5},
		{"req:cancel", "Cancel", 0},
	}
	for _, tt := range tests {
		t.Run(tt.payload, func(t *testing.T) {
			f := newRouterFixture()
			f.manage.res = requests.ManageResult{Kind: requests.ManageCancelled}
			f.r.Process(context.Background(), dmCallback(tt.payload))
			assert.Equal(t, []string{tt.call}, f.manage.calls)
			assert.Equal(t, []int64{tt.id}, f.manage.ids)
			assert.Len(t, f.out.answers, 1)
		})
	}
}

func TestRouter_ManageBadCallbacks(t *testing.T) {
	for _, payload := range []string{"req:open:x", "req:open", "req:unknown:1"} {
		t.Run(payload, func(t *testing.T) {
			f := newRouterFixture()
			f.r.Process(context.Background(), dmCallback(payload))
			assert.Empty(t, f.manage.calls)
			assert.Equal(t, []answer{{"cb1", NotifyUnavailable}}, f.out.answers)
		})
	}
}

func TestRouter_AuthorCard(t *testing.T) {
	open := request.Request{ID: 5, Type: requesttype.Complaint, Status: request.StatusOpen, Body: "Курят", IsAnonymous: true}
	closed := open
	closed.Status, closed.ResultText = request.StatusClosed, "Договорились"

	t.Run("активная — с действиями", func(t *testing.T) {
		f := newRouterFixture()
		f.manage.res = requests.ManageResult{Kind: requests.ManageCard, Card: usecase.RequestCard{Request: open, Confirms: 3, Refutes: 1}}
		f.r.Process(context.Background(), dmCallback("req:open:5"))

		require.Len(t, f.out.dm, 1)
		assert.Equal(t, "Заявка №5 · Жалоба · идёт голосование\n\nКурят\n\nПодтверждаю: 3 · Опровергаю: 1", f.out.dm[0].text)
		labels, payloads := buttons(f.out.dm[0].kb)
		assert.Equal(t, [][]string{{fmt.Sprintf(BtnMaterials, 0)}, {BtnBroadcast}, {BtnEditRequest, BtnCloseRequest}, {BtnDeleteRequest}, {BtnToMenu}}, labels)
		assert.Equal(t, []string{"mat:list:5:0", "bc:start:5", "req:edit:5", "req:close:5", "req:delete:5", "menu:main"}, payloads)
	})

	t.Run("закрытая — итог, без правки", func(t *testing.T) {
		f := newRouterFixture()
		f.manage.res = requests.ManageResult{Kind: requests.ManageCard, Card: usecase.RequestCard{Request: closed}}
		f.r.Process(context.Background(), dmCallback("req:open:5"))

		require.Len(t, f.out.dm, 1)
		assert.Contains(t, f.out.dm[0].text, fmt.Sprintf(AuthorCardResult, "Договорились"))
		labels, _ := buttons(f.out.dm[0].kb)
		assert.Equal(t, [][]string{{fmt.Sprintf(BtnMaterials, 0)}, {BtnBroadcast}, {BtnToMenu}}, labels)
	})
}

func TestRouter_ShowManage(t *testing.T) {
	req := request.Request{ID: 5}
	tests := []struct {
		name       string
		res        requests.ManageResult
		wantText   string
		wantLabels [][]string
		toMenu     bool
	}{
		{name: "ввод итога", res: requests.ManageResult{Kind: requests.ManageAskResult, Card: usecase.RequestCard{Request: req}},
			wantText: fmt.Sprintf(AskResult, 5), wantLabels: [][]string{{BtnCancel}}},
		{name: "ввод текста с ошибкой", res: requests.ManageResult{Kind: requests.ManageAskBody, Problem: request.ErrEmptyBody, Card: usecase.RequestCard{Request: req}},
			wantText: ProblemEmptyBody + "\n\n" + fmt.Sprintf(AskBody, 5), wantLabels: [][]string{{BtnCancel}}},
		{name: "подтверждение удаления", res: requests.ManageResult{Kind: requests.ManageConfirmDelete, Card: usecase.RequestCard{Request: req}},
			wantText: fmt.Sprintf(ConfirmDelete, 5), wantLabels: [][]string{{BtnConfirmDelete, BtnCancel}}},
		{name: "закрыта", res: requests.ManageResult{Kind: requests.ManageClosed, ChatSynced: true, Card: usecase.RequestCard{Request: req}},
			wantText: fmt.Sprintf(RequestClosed, 5), toMenu: true},
		{name: "изменена, чат не обновлён", res: requests.ManageResult{Kind: requests.ManageEdited, Card: usecase.RequestCard{Request: req}},
			wantText: fmt.Sprintf(RequestEdited, 5) + "\n\n" + ChatNotSynced, toMenu: true},
		{name: "удалена", res: requests.ManageResult{Kind: requests.ManageDeleted, ChatSynced: true, Card: usecase.RequestCard{Request: req}},
			wantText: fmt.Sprintf(RequestDeleted, 5), toMenu: true},
		{name: "не найдена", res: requests.ManageResult{Kind: requests.ManageNotFound}, wantText: RequestNotFound, toMenu: true},
		{name: "неактивна", res: requests.ManageResult{Kind: requests.ManageNotActive}, wantText: RequestNotActive, toMenu: true},
		{name: "отменено", res: requests.ManageResult{Kind: requests.ManageCancelled}, wantText: ManageCancelled, toMenu: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRouterFixture()
			f.manage.res, f.manage.handled = tt.res, true
			f.dlg.flow = requests.FlowManage
			f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeReady, Chat: chatA}

			f.r.Process(context.Background(), dmMessage("текст"))

			assert.Equal(t, []string{"HandleMessage"}, f.manage.calls, "сообщение ушло сценарию текущего диалога")
			assert.Empty(t, f.create.calls)
			require.NotEmpty(t, f.out.dm)
			assert.Equal(t, tt.wantText, f.out.dm[0].text)
			labels, _ := buttons(f.out.dm[0].kb)
			assert.Equal(t, tt.wantLabels, labels)
			if tt.toMenu {
				assert.Len(t, f.out.dm, 2)
			} else {
				assert.Len(t, f.out.dm, 1)
			}
		})
	}
}

func TestRouter_ManageErrorFallsBackToMenu(t *testing.T) {
	f := newRouterFixture()
	f.manage.err = errors.New("redis down")
	f.dlg.flow = requests.FlowManage
	f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeNotRegistered}

	f.r.Process(context.Background(), dmMessage("текст"))
	assert.Len(t, f.onb.started, 1)
}

func TestCardText_Statuses(t *testing.T) {
	base := request.Request{ID: 1, Type: requesttype.Complaint, Body: "x"}
	tests := []struct {
		status request.Status
		result string
		want   string
	}{
		{request.StatusOpen, "", ""},
		{request.StatusInProgress, "", CardStatusInProgress},
		{request.StatusClosed, "Решено", fmt.Sprintf(CardStatusClosed, "Решено")},
		{request.StatusExpired, "", CardStatusExpired},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			r := base
			r.Status, r.ResultText = tt.status, tt.result
			text := cardText(usecase.RequestCard{Request: r})
			if tt.want == "" {
				assert.NotContains(t, text, CardStatusInProgress)
				return
			}
			assert.Contains(t, text, "\n\n"+tt.want)
		})
	}
}
