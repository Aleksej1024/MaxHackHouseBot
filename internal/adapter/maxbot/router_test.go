package maxbot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/domain/vote"
	"maxhouse/internal/usecase/onboarding"
	"maxhouse/internal/usecase/requests"
	"maxhouse/internal/usecase/voting"
)

// --- Тестовые дублёры ---

type regCall struct {
	method string
	chatID int64
	user   membership.User
}

type fakeRegistration struct {
	calls  []regCall
	status housechat.Status
	err    error
}

func (f *fakeRegistration) BotAdded(_ context.Context, chatID int64, title string) (housechat.Status, error) {
	f.calls = append(f.calls, regCall{method: "BotAdded", chatID: chatID, user: membership.User{Nickname: title}})
	return f.status, f.err
}

func (f *fakeRegistration) BotRemoved(_ context.Context, chatID int64) error {
	f.calls = append(f.calls, regCall{method: "BotRemoved", chatID: chatID})
	return f.err
}

func (f *fakeRegistration) UserAdded(_ context.Context, chatID int64, u membership.User) error {
	f.calls = append(f.calls, regCall{method: "UserAdded", chatID: chatID, user: u})
	return f.err
}

func (f *fakeRegistration) ChatTitleChanged(_ context.Context, chatID int64, title string) error {
	f.calls = append(f.calls, regCall{method: "ChatTitleChanged", chatID: chatID, user: membership.User{Nickname: title}})
	return f.err
}

func (f *fakeRegistration) UserRemoved(_ context.Context, chatID, userID int64) error {
	f.calls = append(f.calls, regCall{method: "UserRemoved", chatID: chatID, user: membership.User{ID: userID}})
	return f.err
}

// stubOnboarding возвращает заготовленный результат и запоминает вызовы.
type stubOnboarding struct {
	res       onboarding.Result
	err       error
	started   []membership.User
	selected  []int64
	switchedN int
}

func (s *stubOnboarding) Start(_ context.Context, u membership.User) (onboarding.Result, error) {
	s.started = append(s.started, u)
	return s.res, s.err
}

func (s *stubOnboarding) SelectChat(_ context.Context, _, chatID int64) (onboarding.Result, error) {
	s.selected = append(s.selected, chatID)
	return s.res, s.err
}

func (s *stubOnboarding) SwitchChat(context.Context, int64) (onboarding.Result, error) {
	s.switchedN++
	return s.res, s.err
}

type sentMessage struct {
	userID int64
	chatID int64
	text   string
	kb     *model.Keyboard
	atts   []request.Attachment
}

type answer struct {
	callbackID   string
	notification string
}

// mockOutbox записывает исходящие вызовы MAX.
type mockOutbox struct {
	chat    []sentMessage
	dm      []sentMessage
	answers []answer
	err     error
}

func (m *mockOutbox) SendChatMessage(_ context.Context, chatID int64, text string) (string, error) {
	m.chat = append(m.chat, sentMessage{chatID: chatID, text: text})
	return "mid.1", m.err
}

func (m *mockOutbox) SendUserMessage(_ context.Context, userID int64, text string, kb *model.Keyboard) error {
	m.dm = append(m.dm, sentMessage{userID: userID, text: text, kb: kb})
	return m.err
}

func (m *mockOutbox) SendUserMedia(_ context.Context, userID int64, text string, atts []request.Attachment, kb *model.Keyboard) error {
	m.dm = append(m.dm, sentMessage{userID: userID, text: text, kb: kb, atts: atts})
	return m.err
}

func (m *mockOutbox) AnswerCallback(_ context.Context, callbackID, notification string) error {
	m.answers = append(m.answers, answer{callbackID, notification})
	return m.err
}

// stubCreation возвращает заготовленный шаг и записывает вызовы.
type stubCreation struct {
	step    requests.Step
	handled bool
	err     error
	calls   []string
	args    []any
}

func (s *stubCreation) record(name string, args ...any) (requests.Step, error) {
	s.calls = append(s.calls, name)
	s.args = append(s.args, args...)
	return s.step, s.err
}

func (s *stubCreation) Begin(_ context.Context, userID int64) (requests.Step, error) {
	return s.record("Begin", userID)
}

func (s *stubCreation) ChooseType(_ context.Context, _ int64, code requesttype.Code) (requests.Step, error) {
	return s.record("ChooseType", code)
}

func (s *stubCreation) HandleMessage(_ context.Context, _ int64, text string, atts []request.Attachment) (requests.Step, bool, error) {
	st, err := s.record("HandleMessage", text, atts)
	return st, s.handled, err
}

func (s *stubCreation) FinishMedia(context.Context, int64) (requests.Step, error) {
	return s.record("FinishMedia")
}

func (s *stubCreation) ChooseAnonymity(_ context.Context, _ int64, anonymous bool) (requests.Step, error) {
	return s.record("ChooseAnonymity", anonymous)
}

func (s *stubCreation) Edit(context.Context, int64) (requests.Step, error) { return s.record("Edit") }

func (s *stubCreation) Publish(_ context.Context, u membership.User) (requests.Step, error) {
	return s.record("Publish", u)
}

func (s *stubCreation) Cancel(context.Context, int64) (requests.Step, error) {
	return s.record("Cancel")
}

// stubVoting возвращает заготовленный итог голосования.
type stubVoting struct {
	res   voting.Result
	err   error
	calls []voteCall
}

type voteCall struct {
	requestID, userID int64
	value             vote.Value
}

func (s *stubVoting) Vote(_ context.Context, requestID, userID int64, value vote.Value) (voting.Result, error) {
	s.calls = append(s.calls, voteCall{requestID, userID, value})
	return s.res, s.err
}

// stubManagement возвращает заготовленный результат и записывает вызовы.
type stubManagement struct {
	res     requests.ManageResult
	handled bool
	err     error
	calls   []string
	ids     []int64
}

func (s *stubManagement) rec(name string, id int64) (requests.ManageResult, error) {
	s.calls = append(s.calls, name)
	s.ids = append(s.ids, id)
	return s.res, s.err
}

func (s *stubManagement) MyRequests(_ context.Context, _ int64) (requests.ManageResult, error) {
	return s.rec("MyRequests", 0)
}

func (s *stubManagement) Open(_ context.Context, id, _ int64) (requests.ManageResult, error) {
	return s.rec("Open", id)
}

func (s *stubManagement) StartClose(_ context.Context, id, _ int64) (requests.ManageResult, error) {
	return s.rec("StartClose", id)
}

func (s *stubManagement) StartEdit(_ context.Context, id, _ int64) (requests.ManageResult, error) {
	return s.rec("StartEdit", id)
}

func (s *stubManagement) AskDelete(_ context.Context, id, _ int64) (requests.ManageResult, error) {
	return s.rec("AskDelete", id)
}

func (s *stubManagement) Delete(_ context.Context, id, _ int64) (requests.ManageResult, error) {
	return s.rec("Delete", id)
}

func (s *stubManagement) Cancel(context.Context, int64) (requests.ManageResult, error) {
	return s.rec("Cancel", 0)
}

func (s *stubManagement) HandleMessage(context.Context, int64, string) (requests.ManageResult, bool, error) {
	res, err := s.rec("HandleMessage", 0)
	return res, s.handled, err
}

type routerFixture struct {
	dlg    *stubDialogs
	mat    *stubMaterials
	bc     *stubBroadcasts
	set    *stubSettings
	prob   *stubProblems
	manage *stubManagement
	vote   *stubVoting
	reg    *fakeRegistration
	onb    *stubOnboarding
	create *stubCreation
	out    *mockOutbox
	r      *Router
}

func newRouterFixture() *routerFixture {
	f := &routerFixture{reg: &fakeRegistration{}, onb: &stubOnboarding{}, create: &stubCreation{}, manage: &stubManagement{}, vote: &stubVoting{}, out: &mockOutbox{},
		dlg: &stubDialogs{}, mat: &stubMaterials{}, bc: &stubBroadcasts{}, set: &stubSettings{}, prob: &stubProblems{}}
	f.r = NewRouter(Scenarios{
		Registration: f.reg,
		Onboarding:   f.onb,
		Creation:     f.create,
		Management:   f.manage,
		Dialogs:      f.dlg,
		Materials:    f.mat,
		Broadcasts:   f.bc,
		Settings:     f.set,
		Problems:     f.prob,
		Voting:       f.vote,
	}, f.out, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return f
}

// buttons разворачивает клавиатуру в строки подписей и payload.
func buttons(kb *model.Keyboard) (labels [][]string, payloads []string) {
	if kb == nil {
		return nil, nil
	}
	for _, row := range kb.Build().Payload.Buttons {
		var line []string
		for _, b := range row {
			line = append(line, b.Text)
			payloads = append(payloads, b.Payload)
		}
		labels = append(labels, line)
	}
	return labels, payloads
}

const userID = int64(42)

var (
	chatA = housechat.Chat{ID: 100, Title: "Дом А", Status: housechat.StatusActive}
	chatB = housechat.Chat{ID: 200, Status: housechat.StatusActive}
)

func dmMessage(text string) model.Update {
	return model.Update{
		UpdateType: model.UpdateMessageCreated,
		ChatID:     555,
		UserID:     userID,
		User:       &model.User{UserID: userID, Name: "Иван"},
		Message: &model.MessageUpdate{
			Recipient: model.Recipient{ChatID: 555, ChatType: model.ChatTypeDialog},
			Sender:    model.Sender{UserID: userID},
			Body:      model.MessageBody{Text: text},
		},
	}
}

func dmCallback(payload string) model.Update {
	return model.Update{
		UpdateType: model.UpdateMessageCallback,
		ChatID:     555,
		UserID:     userID,
		Callback:   &model.Callback{CallbackID: "cb1", Payload: payload, User: model.User{UserID: userID}},
		Message: &model.MessageUpdate{
			Recipient: model.Recipient{ChatID: 555, ChatType: model.ChatTypeDialog, UserID: userID},
		},
	}
}

// --- Регистрация (раздел 6) ---

func TestRouter_BotAdded(t *testing.T) {
	tests := []struct {
		name     string
		status   housechat.Status
		err      error
		wantText string
	}{
		{name: "активный чат — приветствие", status: housechat.StatusActive, wantText: ChatGreeting},
		{name: "pending — ожидание подтверждения", status: housechat.StatusPending, wantText: ChatPending},
		{name: "ошибка регистрации — без сообщения", err: errors.New("pg down")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRouterFixture()
			f.reg.status, f.reg.err = tt.status, tt.err

			f.r.Process(context.Background(), model.Update{
				UpdateType: model.UpdateBotAdded,
				ChatID:     -100,
				ChatProp:   &model.ChatProp{Title: "Дом"},
			})

			require.Len(t, f.reg.calls, 1)
			assert.Equal(t, regCall{method: "BotAdded", chatID: -100, user: membership.User{Nickname: "Дом"}}, f.reg.calls[0])
			if tt.wantText == "" {
				assert.Empty(t, f.out.chat)
				return
			}
			require.Len(t, f.out.chat, 1)
			assert.Equal(t, sentMessage{chatID: -100, text: tt.wantText}, f.out.chat[0])
		})
	}
}

func TestRouter_MembershipEvents(t *testing.T) {
	f := newRouterFixture()
	ctx := context.Background()

	f.r.Process(ctx, model.Update{UpdateType: model.UpdateUserAdded, ChatID: -100, UserID: 7,
		User: &model.User{UserID: 7, Username: "nick"}})
	f.r.Process(ctx, model.Update{UpdateType: model.UpdateUserAdded, ChatID: -100, UserID: 8,
		User: &model.User{UserID: 8, IsBot: true}})
	f.r.Process(ctx, model.Update{UpdateType: model.UpdateUserRemoved, ChatID: -100, UserID: 7})
	f.r.Process(ctx, model.Update{UpdateType: model.UpdateBotRemoved, ChatID: -100})
	f.r.Process(ctx, model.Update{UpdateType: model.UpdateChatTitleChanged, ChatID: -100,
		ChatProp: &model.ChatProp{Title: "Новый дом"}})

	assert.Equal(t, []regCall{
		{method: "UserAdded", chatID: -100, user: membership.User{ID: 7, Nickname: "nick"}},
		{method: "UserRemoved", chatID: -100, user: membership.User{ID: 7}},
		{method: "BotRemoved", chatID: -100},
		{method: "ChatTitleChanged", chatID: -100, user: membership.User{Nickname: "Новый дом"}},
	}, f.reg.calls, "вход бота как участника пропускается")
}

// --- Первое сообщение (раздел 7) ---

func TestRouter_DialogNotRegistered(t *testing.T) {
	f := newRouterFixture()
	f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeNotRegistered}

	f.r.Process(context.Background(), dmMessage("привет"))

	require.Len(t, f.onb.started, 1)
	assert.Equal(t, membership.User{ID: userID, Nickname: "Иван"}, f.onb.started[0])
	require.Len(t, f.out.dm, 1)
	assert.Equal(t, userID, f.out.dm[0].userID)
	assert.Equal(t, DMNotRegistered, f.out.dm[0].text)
	assert.Nil(t, f.out.dm[0].kb)
}

func TestRouter_DialogChooseChat(t *testing.T) {
	f := newRouterFixture()
	f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeChooseChat, Chats: []housechat.Chat{chatA, chatB}}

	f.r.Process(context.Background(), model.Update{
		UpdateType: model.UpdateBotStarted,
		ChatID:     555,
		UserID:     userID,
		User:       &model.User{UserID: userID, FirstName: "Иван"},
	})

	require.Len(t, f.out.dm, 1)
	assert.Equal(t, DMChooseChat, f.out.dm[0].text)
	labels, payloads := buttons(f.out.dm[0].kb)
	assert.Equal(t, [][]string{{"Дом А"}, {fmt.Sprintf(ChatUntitled, 200)}}, labels)
	assert.Equal(t, []string{"home:100", "home:200"}, payloads)
}

func TestRouter_DialogReady(t *testing.T) {
	tests := []struct {
		name       string
		res        onboarding.Result
		wantText   string
		wantLabels [][]string
	}{
		{
			name:     "первое обращение, один дом",
			res:      onboarding.Result{Outcome: onboarding.OutcomeReady, Chat: chatA, Chats: []housechat.Chat{chatA}, Greet: true},
			wantText: fmt.Sprintf(DMGreeting, "Дом А"),
			wantLabels: [][]string{
				{BtnCreateRequest},
				{BtnReportProblem},
				{BtnMyRequests, BtnHouseRequests},
				{BtnSettings, BtnHelp},
			},
		},
		{
			name:     "повторное обращение, несколько домов",
			res:      onboarding.Result{Outcome: onboarding.OutcomeReady, Chat: chatA, Chats: []housechat.Chat{chatA, chatB}},
			wantText: fmt.Sprintf(DMMenu, "Дом А"),
			wantLabels: [][]string{
				{BtnCreateRequest},
				{BtnReportProblem},
				{BtnMyRequests, BtnHouseRequests},
				{BtnSettings, BtnHelp},
				{BtnSwitchHome},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRouterFixture()
			f.onb.res = tt.res

			f.r.Process(context.Background(), dmMessage("меню"))

			require.Len(t, f.out.dm, 1)
			assert.Equal(t, tt.wantText, f.out.dm[0].text)
			labels, payloads := buttons(f.out.dm[0].kb)
			assert.Equal(t, tt.wantLabels, labels)
			for _, p := range payloads {
				action, _ := parseCallback(p)
				assert.Equal(t, cbMenu, action, "в payload только код действия")
			}
		})
	}
}

func TestRouter_IgnoresGroupAndBotMessages(t *testing.T) {
	f := newRouterFixture()

	group := dmMessage("сообщение в домовом чате")
	group.Message.Recipient.ChatType = model.ChatTypeChat
	f.r.Process(context.Background(), group)

	fromBot := dmMessage("от бота")
	fromBot.Message.Sender.IsBot = true
	f.r.Process(context.Background(), fromBot)

	assert.Empty(t, f.onb.started)
	assert.Empty(t, f.out.dm)
}

func TestRouter_OnboardingErrorNoMessage(t *testing.T) {
	f := newRouterFixture()
	f.onb.err = errors.New("pg down")

	f.r.Process(context.Background(), dmMessage("привет"))
	assert.Empty(t, f.out.dm)
}

// --- Кнопки лички ---

func TestRouter_CallbackSelectHome(t *testing.T) {
	f := newRouterFixture()
	f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeReady, Chat: chatA, Chats: []housechat.Chat{chatA, chatB}, Greet: true}

	f.r.Process(context.Background(), dmCallback("home:100"))

	assert.Equal(t, []int64{100}, f.onb.selected)
	assert.Equal(t, []answer{{"cb1", NotifyHomeChosen}}, f.out.answers)
	require.Len(t, f.out.dm, 1)
	assert.Equal(t, fmt.Sprintf(DMGreeting, "Дом А"), f.out.dm[0].text)
}

func TestRouter_CallbackSelectForeignHome(t *testing.T) {
	f := newRouterFixture()
	f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeChooseChat, Chats: []housechat.Chat{chatA, chatB}}

	f.r.Process(context.Background(), dmCallback("home:999"))

	assert.Equal(t, []answer{{"cb1", NotifyUnavailable}}, f.out.answers)
	require.Len(t, f.out.dm, 1)
	assert.Equal(t, DMChooseChat, f.out.dm[0].text)
}

func TestRouter_CallbackBadPayload(t *testing.T) {
	for _, payload := range []string{"home:abc", "unknown:1", "", "menu:nope"} {
		t.Run(payload, func(t *testing.T) {
			f := newRouterFixture()
			f.r.Process(context.Background(), dmCallback(payload))

			assert.Empty(t, f.onb.selected)
			assert.Equal(t, []answer{{"cb1", NotifyUnavailable}}, f.out.answers)
			assert.Empty(t, f.out.dm)
		})
	}
}

func TestRouter_CallbackMenu(t *testing.T) {
	t.Run("помощь", func(t *testing.T) {
		f := newRouterFixture()
		f.r.Process(context.Background(), dmCallback("menu:help"))
		require.Len(t, f.out.dm, 1)
		assert.Equal(t, helpText(), f.out.dm[0].text)
		assert.Len(t, f.out.answers, 1)
	})

	t.Run("сменить дом", func(t *testing.T) {
		f := newRouterFixture()
		f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeChooseChat, Chats: []housechat.Chat{chatA, chatB}}
		f.r.Process(context.Background(), dmCallback("menu:switch"))
		assert.Equal(t, 1, f.onb.switchedN)
		require.Len(t, f.out.dm, 1)
		assert.Equal(t, DMChooseChat, f.out.dm[0].text)
	})
}

func TestRouter_SendErrorsDoNotPanic(t *testing.T) {
	f := newRouterFixture()
	f.out.err = errors.New("max down")
	f.onb.res = onboarding.Result{Outcome: onboarding.OutcomeNotRegistered}

	f.r.Process(context.Background(), dmMessage("привет"))
	f.r.Process(context.Background(), dmCallback("menu:help"))
	assert.Len(t, f.out.dm, 2)
}

func TestParseCallback(t *testing.T) {
	tests := []struct{ in, action, param string }{
		{"home:100", "home", "100"},
		{"home:-100", "home", "-100"},
		{"v:confirm:123", "v", "confirm:123"},
		{"menu", "menu", ""},
		{"", "", ""},
	}
	for _, tt := range tests {
		action, param := parseCallback(tt.in)
		assert.Equal(t, tt.action, action, tt.in)
		assert.Equal(t, tt.param, param, tt.in)
	}
}
