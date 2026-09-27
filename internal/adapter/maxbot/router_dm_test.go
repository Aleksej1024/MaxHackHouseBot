package maxbot

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	maxbotapi "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/broadcast"
	"maxhouse/internal/domain/comment"
	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/usecase"
	"maxhouse/internal/usecase/notify"
	"maxhouse/internal/usecase/onboarding"
	"maxhouse/internal/usecase/requests"
	"maxhouse/internal/usecase/settings"
)

// --- Заглушки сценариев этапа 6 ---

type stubDialogs struct {
	flow     requests.Flow
	err      error
	resets   int
	resetErr error
}

func (s *stubDialogs) Current(context.Context, int64) (requests.Flow, error) { return s.flow, s.err }

func (s *stubDialogs) Reset(context.Context, int64) error {
	s.resets++
	s.flow = requests.FlowNone
	return s.resetErr
}

type stubMaterials struct {
	res     requests.MaterialResult
	handled bool
	err     error
	calls   []string
	args    []any
}

func (s *stubMaterials) rec(name string, args ...any) (requests.MaterialResult, error) {
	s.calls = append(s.calls, name)
	s.args = append(s.args, args...)
	return s.res, s.err
}

func (s *stubMaterials) HouseRequests(context.Context, int64) (requests.MaterialResult, error) {
	return s.rec("HouseRequests")
}
func (s *stubMaterials) View(_ context.Context, id, _ int64) (requests.MaterialResult, error) {
	return s.rec("View", id)
}
func (s *stubMaterials) StartComment(_ context.Context, id, _ int64) (requests.MaterialResult, error) {
	return s.rec("StartComment", id)
}
func (s *stubMaterials) StartEvidence(_ context.Context, id, _ int64) (requests.MaterialResult, error) {
	return s.rec("StartEvidence", id)
}
func (s *stubMaterials) Cancel(context.Context, int64) (requests.MaterialResult, error) {
	return s.rec("Cancel")
}
func (s *stubMaterials) HandleMessage(_ context.Context, u membership.User, text string, atts []request.Attachment) (requests.MaterialResult, bool, error) {
	res, err := s.rec("HandleMessage", u, text, atts)
	return res, s.handled, err
}
func (s *stubMaterials) ListMaterials(_ context.Context, id, _ int64, offset int) (requests.MaterialResult, error) {
	return s.rec("ListMaterials", id, offset)
}

type stubBroadcasts struct {
	res     requests.BroadcastResult
	handled bool
	err     error
	calls   []string
	args    []any
}

func (s *stubBroadcasts) rec(name string, args ...any) (requests.BroadcastResult, error) {
	s.calls = append(s.calls, name)
	s.args = append(s.args, args...)
	return s.res, s.err
}

func (s *stubBroadcasts) Start(_ context.Context, id, _ int64) (requests.BroadcastResult, error) {
	return s.rec("Start", id)
}
func (s *stubBroadcasts) ChooseAudience(_ context.Context, id, _ int64, aud broadcast.Audience) (requests.BroadcastResult, error) {
	return s.rec("ChooseAudience", id, aud)
}
func (s *stubBroadcasts) HandleMessage(_ context.Context, _ int64, text string) (requests.BroadcastResult, bool, error) {
	res, err := s.rec("HandleMessage", text)
	return res, s.handled, err
}
func (s *stubBroadcasts) Send(context.Context, int64) (requests.BroadcastResult, error) {
	return s.rec("Send")
}
func (s *stubBroadcasts) Cancel(context.Context, int64) (requests.BroadcastResult, error) {
	return s.rec("Cancel")
}

type stubSettings struct {
	res   settings.Result
	err   error
	calls []string
}

func (s *stubSettings) Get(context.Context, int64) (settings.Result, error) {
	s.calls = append(s.calls, "Get")
	return s.res, s.err
}
func (s *stubSettings) ToggleForward(context.Context, int64) (settings.Result, error) {
	s.calls = append(s.calls, "ToggleForward")
	return s.res, s.err
}
func (s *stubSettings) ToggleNotifyMaterials(context.Context, int64) (settings.Result, error) {
	s.calls = append(s.calls, "ToggleNotifyMaterials")
	return s.res, s.err
}

var (
	openReq = request.Request{ID: 5, Type: requesttype.Poll, Status: request.StatusOpen, Body: "Шлагбаум?"}
	photoAt = request.Attachment{Kind: request.KindImage, Ref: "t1"}
)

// --- Открытие заявки ---

func TestParseRequestNumber(t *testing.T) {
	for in, want := range map[string]int64{"12": 12, " №7 ": 7, "№ 15": 15} {
		got, ok := parseRequestNumber(in)
		assert.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"", "0", "абв", "12 и 13", "-3", "№"} {
		_, ok := parseRequestNumber(in)
		assert.False(t, ok, in)
	}
}

func TestRouter_OpenByNumber(t *testing.T) {
	f := newRouterFixture()
	f.mat.res = requests.MaterialResult{
		Kind:          requests.MatNeighborCard,
		Card:          usecase.RequestCard{Request: openReq, AuthorNickname: "автор", Confirms: 2},
		CanContribute: true,
	}

	f.r.Process(context.Background(), dmMessage("№5"))

	assert.Equal(t, []string{"View"}, f.mat.calls)
	assert.Equal(t, []any{int64(5)}, f.mat.args)
	require.Len(t, f.out.dm, 1)
	assert.Equal(t, "Заявка №5 · Опрос · идёт голосование\n\nШлагбаум?\n\nАвтор: автор\nДа: 2 · Нет: 0", f.out.dm[0].text)
	_, payloads := buttons(f.out.dm[0].kb)
	assert.Equal(t, []string{"mat:comment:5", "mat:evidence:5", "menu:main"}, payloads)
	assert.Empty(t, f.onb.started)
}

func TestNeighborCard_AnonymousAndClosed(t *testing.T) {
	r := openReq
	r.IsAnonymous, r.Status, r.ResultText = true, request.StatusClosed, "Поставили"
	text := neighborCardText(usecase.RequestCard{Request: r, AuthorNickname: "автор"})
	assert.Contains(t, text, CardAuthorAnonymous)
	assert.NotContains(t, text, "автор\n", "ник анонимного автора не показывается")
	assert.Contains(t, text, fmt.Sprintf(CardStatusClosed, "Поставили"))
	labels, _ := buttons(neighborCardKeyboard(5, false))
	assert.Equal(t, [][]string{{BtnToMenu}}, labels, "к закрытой заявке — только «В меню»")
}

func TestRouter_OpenFromChat(t *testing.T) {
	f := newRouterFixture()
	f.mat.res = requests.MaterialResult{Kind: requests.MatNeighborCard, Card: usecase.RequestCard{Request: openReq}}

	f.r.Process(context.Background(), chatCallback("open:5"))

	require.Len(t, f.out.dm, 1, "карточка отправлена в личку нажавшему")
	assert.Equal(t, userID, f.out.dm[0].userID)
	assert.Equal(t, []answer{{"cb1", NotifyCardSent}}, f.out.answers)
}

func TestRouter_OpenFromChat_NoDialog(t *testing.T) {
	f := newRouterFixture()
	f.mat.res = requests.MaterialResult{Kind: requests.MatNeighborCard, Card: usecase.RequestCard{Request: openReq}}
	f.out.err = errors.New("dialog not found")

	f.r.Process(context.Background(), chatCallback("open:5"))
	assert.Equal(t, []answer{{"cb1", NotifyStartDialog}}, f.out.answers)
}

func TestRouter_OpenFromChat_NotMember(t *testing.T) {
	f := newRouterFixture()
	f.mat.res = requests.MaterialResult{Kind: requests.MatNotFound}

	f.r.Process(context.Background(), chatCallback("open:5"))
	assert.Empty(t, f.out.dm)
	assert.Equal(t, []answer{{"cb1", NotifyUnavailable}}, f.out.answers)
}

func TestRouter_AuthorCardHasMaterialsAndBroadcast(t *testing.T) {
	f := newRouterFixture()
	closed := openReq
	closed.Status = request.StatusClosed
	f.mat.res = requests.MaterialResult{Kind: requests.MatAuthorCard, Card: usecase.RequestCard{Request: closed, Materials: 3}}

	f.r.Process(context.Background(), dmCallback("open:5"))

	require.Len(t, f.out.dm, 1)
	labels, payloads := buttons(f.out.dm[0].kb)
	assert.Equal(t, [][]string{{fmt.Sprintf(BtnMaterials, 3)}, {BtnBroadcast}, {BtnToMenu}}, labels,
		"у закрытой заявки — только материалы и рассылка")
	assert.Equal(t, []string{"mat:list:5:0", "bc:start:5", "menu:main"}, payloads)
}

// --- Материалы ---

func TestRouter_MaterialCallbacks(t *testing.T) {
	tests := []struct {
		payload string
		call    string
		args    []any
	}{
		{"mat:comment:5", "StartComment", []any{int64(5)}},
		{"mat:evidence:5", "StartEvidence", []any{int64(5)}},
		{"mat:list:5:10", "ListMaterials", []any{int64(5), 10}},
		{"mat:cancel", "Cancel", nil},
	}
	for _, tt := range tests {
		t.Run(tt.payload, func(t *testing.T) {
			f := newRouterFixture()
			f.mat.res = requests.MaterialResult{Kind: requests.MatCancelled}
			f.r.Process(context.Background(), dmCallback(tt.payload))
			assert.Equal(t, []string{tt.call}, f.mat.calls)
			assert.Equal(t, tt.args, f.mat.args)
		})
	}
	for _, bad := range []string{"mat:list:5:x", "mat:list:5:-1", "mat:comment:x", "mat:zzz:5"} {
		f := newRouterFixture()
		f.r.Process(context.Background(), dmCallback(bad))
		assert.Empty(t, f.mat.calls, bad)
		assert.Equal(t, []answer{{"cb1", NotifyUnavailable}}, f.out.answers, bad)
	}
}

func TestRouter_MaterialDialog(t *testing.T) {
	f := newRouterFixture()
	f.dlg.flow = requests.FlowMaterial
	f.mat.handled = true
	f.mat.res = requests.MaterialResult{Kind: requests.MatAskEvidence, Problem: comment.ErrNoMedia, Card: usecase.RequestCard{Request: openReq}}
	upd := dmMessage("подпись")

	f.r.Process(context.Background(), upd)

	assert.Equal(t, []string{"HandleMessage"}, f.mat.calls)
	assert.Equal(t, membership.User{ID: userID, Nickname: "Иван"}, f.mat.args[0], "ник отправителя передаётся")
	require.Len(t, f.out.dm, 1)
	assert.Equal(t, ProblemNoMedia+"\n\n"+fmt.Sprintf(EvidenceHint, 5), f.out.dm[0].text)
	labels, _ := buttons(f.out.dm[0].kb)
	assert.Equal(t, [][]string{{BtnCancel}}, labels)
}

func TestRouter_CommentHintMentionsNickname(t *testing.T) {
	assert.Contains(t, CommentHint, "ник", "отправитель предупреждён, что автор увидит ник")
	assert.Contains(t, EvidenceHint, "ник")
}

func TestRouter_MaterialsPage(t *testing.T) {
	f := newRouterFixture()
	f.r.loc = time.FixedZone("MSK", 3*3600)
	at := time.Date(2026, 9, 23, 9, 5, 0, 0, time.UTC)
	f.mat.res = requests.MaterialResult{
		Kind: requests.MatMaterials,
		Card: usecase.RequestCard{Request: openReq},
		Comments: []comment.Comment{
			{ID: 1, Kind: comment.KindComment, AuthorNickname: "сосед", Body: "видел", CreatedAt: at},
			{ID: 2, Kind: comment.KindEvidence, AuthorNickname: "другой", Attachments: []request.Attachment{photoAt}, CreatedAt: at},
		},
		Offset: 0,
		Total:  3,
	}

	f.r.Process(context.Background(), dmCallback("mat:list:5:0"))

	require.Len(t, f.out.dm, 3)
	assert.Equal(t, "Комментарий · сосед · 23.09.2026 12:05\n\nвидел", f.out.dm[0].text, "время в DISPLAY_TIMEZONE")
	assert.Equal(t, []request.Attachment{photoAt}, f.out.dm[1].atts, "вложения доказательства пересылаются автору")
	assert.Equal(t, fmt.Sprintf(MaterialsPage, 1, 2, 3), f.out.dm[2].text)
	_, payloads := buttons(f.out.dm[2].kb)
	assert.Equal(t, []string{"mat:list:5:2", "menu:main"}, payloads)
}

func TestRouter_MaterialsEmpty(t *testing.T) {
	f := newRouterFixture()
	f.mat.res = requests.MaterialResult{Kind: requests.MatMaterials, Card: usecase.RequestCard{Request: openReq}}

	f.r.Process(context.Background(), dmCallback("mat:list:5:0"))
	require.Len(t, f.out.dm, 1)
	assert.Equal(t, fmt.Sprintf(MaterialsEmpty, 5), f.out.dm[0].text)
}

func TestRouter_HouseRequests(t *testing.T) {
	f := newRouterFixture()
	f.mat.res = requests.MaterialResult{Kind: requests.MatHouseList, Chat: chatA, Requests: []request.Request{openReq}}

	f.r.Process(context.Background(), dmCallback("menu:house"))

	require.Len(t, f.out.dm, 1)
	assert.Equal(t, fmt.Sprintf(HouseRequestsTitle, "Дом А"), f.out.dm[0].text)
	_, payloads := buttons(f.out.dm[0].kb)
	assert.Equal(t, []string{"open:5", "menu:main"}, payloads)
}

// --- Рассылка ---

func TestRouter_BroadcastFlow(t *testing.T) {
	t.Run("выбор аудитории — подписи по типу", func(t *testing.T) {
		f := newRouterFixture()
		f.bc.res = requests.BroadcastResult{Kind: requests.BcChooseAudience, Request: openReq}
		f.r.Process(context.Background(), dmCallback("bc:start:5"))

		assert.Equal(t, []string{"Start"}, f.bc.calls)
		require.Len(t, f.out.dm, 1)
		labels, payloads := buttons(f.out.dm[0].kb)
		assert.Equal(t, [][]string{{"Все ответившие"}, {"Ответившие «Да»"}, {"Ответившие «Нет»"}, {BtnCancel}}, labels)
		assert.Equal(t, []string{"bc:aud:5:all", "bc:aud:5:confirm", "bc:aud:5:refute", "bc:cancel"}, payloads)
	})

	t.Run("аудитория", func(t *testing.T) {
		f := newRouterFixture()
		f.bc.res = requests.BroadcastResult{Kind: requests.BcAskText, Request: openReq, Audience: broadcast.AudienceRefute}
		f.r.Process(context.Background(), dmCallback("bc:aud:5:refute"))
		assert.Equal(t, []any{int64(5), broadcast.AudienceRefute}, f.bc.args)
		assert.Equal(t, fmt.Sprintf(BroadcastAskText, "Ответившие «Нет»"), f.out.dm[0].text)
	})

	t.Run("подтверждение из диалога", func(t *testing.T) {
		f := newRouterFixture()
		f.dlg.flow, f.bc.handled = requests.FlowBroadcast, true
		f.bc.res = requests.BroadcastResult{Kind: requests.BcConfirm, Request: openReq, Audience: broadcast.AudienceAll, Recipients: 4, Text: "Собрание"}
		f.r.Process(context.Background(), dmMessage("Собрание"))

		require.Len(t, f.out.dm, 1)
		assert.Equal(t, fmt.Sprintf(BroadcastConfirm, 5, "Все ответившие", 4, "Собрание"), f.out.dm[0].text)
		_, payloads := buttons(f.out.dm[0].kb)
		assert.Equal(t, []string{"bc:send", "bc:cancel"}, payloads)
	})

	t.Run("отправлено, часть не доставлена", func(t *testing.T) {
		f := newRouterFixture()
		f.bc.res = requests.BroadcastResult{Kind: requests.BcSent, Request: openReq, Recipients: 4,
			Delivery: notify.Delivery{Delivered: 3, Unavailable: 1}}
		f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeReady, Chat: chatA}
		f.r.Process(context.Background(), dmCallback("bc:send"))

		require.Len(t, f.out.dm, 2, "итог и меню")
		assert.Equal(t, fmt.Sprintf(BroadcastSent, 5, 3, 4)+"\n\n"+BroadcastUndelivered, f.out.dm[0].text)
	})

	t.Run("кулдаун", func(t *testing.T) {
		f := newRouterFixture()
		next := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
		f.bc.res = requests.BroadcastResult{Kind: requests.BcCooldown, Request: openReq, NextAt: next}
		f.r.Process(context.Background(), dmCallback("bc:start:5"))
		assert.Equal(t, []answer{{"cb1", NotifyUnavailable}}, f.out.answers)
		assert.Equal(t, fmt.Sprintf(BroadcastCooldown, 5, "24.09.2026 10:00"), f.out.dm[0].text)
	})

	for _, bad := range []string{"bc:aud:5:nobody", "bc:start:x", "bc:zzz"} {
		f := newRouterFixture()
		f.r.Process(context.Background(), dmCallback(bad))
		assert.Empty(t, f.bc.calls, bad)
	}
}

// --- Настройки ---

func TestRouter_Settings(t *testing.T) {
	f := newRouterFixture()
	f.set.res = settings.Result{Chat: housechat.Chat{ID: 1, Title: "Дом А"}, ForwardToDM: true, NotifyMaterials: false, CanCreate: false}

	f.r.Process(context.Background(), dmCallback("menu:settings"))
	require.Len(t, f.out.dm, 1)
	assert.Equal(t, "Настройки для дома «Дом А».\n\n"+
		"Новые заявки дома в личку: включено.\n"+
		"Уведомления о комментариях к моим заявкам: выключено.\n"+
		"Создание заявок: временно ограничено.", f.out.dm[0].text)
	labels, payloads := buttons(f.out.dm[0].kb)
	assert.Equal(t, [][]string{{BtnForwardOff}, {BtnNotifyMatOn}, {BtnToMenu}}, labels, "кнопка — действие, обратное текущему")
	assert.Equal(t, []string{"set:forward", "set:materials", "menu:main"}, payloads)
	assert.NotContains(t, f.out.dm[0].text, "рейтинг", "число рейтинга не показываем")
	assert.NotContains(t, f.out.dm[0].text, "ересылк", "слово «пересылка» путают с рассылкой")

	for payload, call := range map[string]string{"set:forward": "ToggleForward", "set:materials": "ToggleNotifyMaterials"} {
		f = newRouterFixture()
		f.r.Process(context.Background(), dmCallback(payload))
		assert.Equal(t, []string{call}, f.set.calls, payload)
	}
}

func TestRouter_SettingsNoChat(t *testing.T) {
	f := newRouterFixture()
	f.set.res = settings.Result{NoChat: true}
	f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeNotRegistered}

	f.r.Process(context.Background(), dmCallback("menu:settings"))
	require.Len(t, f.out.dm, 1)
	assert.Equal(t, DMNotRegistered, f.out.dm[0].text)
}

// --- Уведомления в личку ---

func TestDMError(t *testing.T) {
	require.NoError(t, dmError(nil))
	apiErr := &maxbotapiError
	require.ErrorIs(t, dmError(apiErr), usecase.ErrDMUnavailable, "постоянная ошибка MAX — личка недоступна")
	other := errors.New("что-то ещё")
	assert.Equal(t, other, dmError(other))
}

var maxbotapiError = maxbotapi.Error{Code: "chat.denied", Message: "dialog not started"}

// --- Кнопка «В меню» ---

func TestRouter_ToMenu(t *testing.T) {
	f := newRouterFixture()
	f.dlg.flow = requests.FlowCreate // незавершённый черновик заявки
	f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeReady, Chat: chatA}

	f.r.Process(context.Background(), dmCallback("menu:main"))

	assert.Equal(t, 1, f.dlg.resets, "диалог прерывается, как по «Отмене»")
	assert.Equal(t, []answer{{"cb1", NotifyOK}}, f.out.answers)
	require.Len(t, f.out.dm, 1)
	assert.Equal(t, fmt.Sprintf(DMMenu, "Дом А"), f.out.dm[0].text)

	// Следующее сообщение уже не уходит в старый черновик.
	f.r.Process(context.Background(), dmMessage("текст"))
	assert.Empty(t, f.create.calls)
}

func TestRouter_ToMenu_ResetErrorStillShowsMenu(t *testing.T) {
	f := newRouterFixture()
	f.dlg.resetErr = errors.New("redis down")
	f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeReady, Chat: chatA}

	f.r.Process(context.Background(), dmCallback("menu:main"))
	require.Len(t, f.out.dm, 1)
	assert.Equal(t, fmt.Sprintf(DMMenu, "Дом А"), f.out.dm[0].text)
}

func TestToMenuButton_OnlyOnScreensWithoutCancel(t *testing.T) {
	hasMenu := func(kb *model.Keyboard) bool {
		_, payloads := buttons(kb)
		for _, p := range payloads {
			if p == "menu:main" {
				return true
			}
		}
		return false
	}
	r := request.Request{ID: 5, Type: requesttype.Complaint, Status: request.StatusOpen}
	// Экраны-тупики: есть «В меню».
	assert.True(t, hasMenu(requestListKeyboard([]request.Request{r})))
	assert.True(t, hasMenu(myRequestsKeyboard([]request.Request{r})))
	assert.True(t, hasMenu(authorCardKeyboard(r, 0)))
	assert.True(t, hasMenu(neighborCardKeyboard(5, true)))
	assert.True(t, hasMenu(settingsKeyboard(false, true)))
	assert.True(t, hasMenu(materialsPageKeyboard(5, 0)))
	assert.True(t, hasMenu(toMenuKeyboard()))
	// Шаги диалогов: «Отмена» уже ведёт в меню, дублировать не нужно.
	assert.False(t, hasMenu(createCancelKeyboard()))
	assert.False(t, hasMenu(matCancelKeyboard()))
	assert.False(t, hasMenu(bcConfirmKeyboard()))
	// В домовом чате меню нет.
	assert.False(t, hasMenu(chatCardKeyboard(r)))
}
