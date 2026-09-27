package maxbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"

	"maxhouse/internal/domain/comment"
	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/domain/vote"
	"maxhouse/internal/usecase"
	"maxhouse/internal/usecase/onboarding"
	"maxhouse/internal/usecase/requests"
	"maxhouse/internal/usecase/voting"
)

// Registration — часть сценария регистрации, нужная роутеру.
// Интерфейс объявлен здесь, чтобы тестировать роутер на дублёрах.
type Registration interface {
	BotAdded(ctx context.Context, chatID int64, title string) (housechat.Status, error)
	BotRemoved(ctx context.Context, chatID int64) error
	UserAdded(ctx context.Context, chatID int64, u membership.User) error
	UserRemoved(ctx context.Context, chatID, userID int64) error
	ChatTitleChanged(ctx context.Context, chatID int64, title string) error
}

// Onboarding — сценарий первого сообщения в личке
type Onboarding interface {
	Start(ctx context.Context, u membership.User) (onboarding.Result, error)
	SelectChat(ctx context.Context, userID, chatID int64) (onboarding.Result, error)
	SwitchChat(ctx context.Context, userID int64) (onboarding.Result, error)
}

// RequestCreation — сценарий создания заявки
type RequestCreation interface {
	Begin(ctx context.Context, userID int64) (requests.Step, error)
	ChooseType(ctx context.Context, userID int64, code requesttype.Code) (requests.Step, error)
	HandleMessage(ctx context.Context, userID int64, text string, atts []request.Attachment) (requests.Step, bool, error)
	FinishMedia(ctx context.Context, userID int64) (requests.Step, error)
	ChooseAnonymity(ctx context.Context, userID int64, anonymous bool) (requests.Step, error)
	Edit(ctx context.Context, userID int64) (requests.Step, error)
	Publish(ctx context.Context, u membership.User) (requests.Step, error)
	Cancel(ctx context.Context, userID int64) (requests.Step, error)
}

// Management — действия автора со своими заявками
type Management interface {
	MyRequests(ctx context.Context, userID int64) (requests.ManageResult, error)
	Open(ctx context.Context, requestID, userID int64) (requests.ManageResult, error)
	StartClose(ctx context.Context, requestID, userID int64) (requests.ManageResult, error)
	StartEdit(ctx context.Context, requestID, userID int64) (requests.ManageResult, error)
	AskDelete(ctx context.Context, requestID, userID int64) (requests.ManageResult, error)
	Delete(ctx context.Context, requestID, userID int64) (requests.ManageResult, error)
	Cancel(ctx context.Context, userID int64) (requests.ManageResult, error)
	HandleMessage(ctx context.Context, userID int64, text string) (requests.ManageResult, bool, error)
}

// Voting — голосование по заявкам.
type Voting interface {
	Vote(ctx context.Context, requestID, userID int64, value vote.Value) (voting.Result, error)
}

// Outbox — исходящие вызовы MAX, нужные роутеру (реализует Messenger).
type Outbox interface {
	SendChatMessage(ctx context.Context, chatID int64, text string) (string, error)
	SendUserMessage(ctx context.Context, userID int64, text string, kb *model.Keyboard) error
	SendUserMedia(ctx context.Context, userID int64, text string, atts []request.Attachment, kb *model.Keyboard) error
	AnswerCallback(ctx context.Context, callbackID, notification string) error
}

// Router реализует webhook.Processor: разбирает апдейт MAX и вызывает
// соответствующий сценарий. Ошибки логируются, апдейт считается обработанным
// (обработчики идемпотентны, MAX пришлёт повтор при сбое доставки).
//
// В логи пишутся только id и типы событий, без ников и текстов (раздел 12).
type Router struct {
	reg    Registration
	onb    Onboarding
	create RequestCreation
	manage Management
	voting Voting
	dlg    Dialogs
	mat    MaterialsScenario
	bc     BroadcastScenario
	set    SettingsScenario
	prob   ProblemScenario
	out    Outbox
	log    *slog.Logger
	loc    *time.Location
}

// Scenarios — сценарии, которые вызывает роутер.
type Scenarios struct {
	Registration Registration
	Onboarding   Onboarding
	Creation     RequestCreation
	Management   Management
	Voting       Voting
	Dialogs      Dialogs
	Materials    MaterialsScenario
	Broadcasts   BroadcastScenario
	Settings     SettingsScenario
	Problems     ProblemScenario
}

// Option — необязательный параметр роутера.
type Option func(r *Router)

// WithLocation — часовой пояс для времени в сообщениях (DISPLAY_TIMEZONE).
func WithLocation(loc *time.Location) Option {
	return func(r *Router) { r.loc = loc }
}

func NewRouter(sc Scenarios, out Outbox, log *slog.Logger, opts ...Option) *Router {
	r := &Router{
		reg:    sc.Registration,
		onb:    sc.Onboarding,
		create: sc.Creation,
		manage: sc.Management,
		voting: sc.Voting,
		dlg:    sc.Dialogs,
		mat:    sc.Materials,
		bc:     sc.Broadcasts,
		set:    sc.Settings,
		prob:   sc.Problems,
		out:    out,
		log:    log,
		loc:    time.UTC,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

func (r *Router) Process(ctx context.Context, upd model.Update) {
	attrs := []any{
		"update_type", string(upd.UpdateType),
		"chat_id", upd.ChatID,
		"user_id", upd.UserID,
	}

	switch upd.UpdateType {
	case model.UpdateBotAdded:
		r.onBotAdded(ctx, upd, attrs)
	case model.UpdateBotRemoved:
		if err := r.reg.BotRemoved(ctx, upd.ChatID); err != nil {
			r.log.Error("обработка удаления бота", append(attrs, "error", err)...)
		}
	case model.UpdateUserAdded:
		r.onUserAdded(ctx, upd, attrs)
	case model.UpdateUserRemoved:
		if err := r.reg.UserRemoved(ctx, upd.ChatID, upd.UserID); err != nil {
			r.log.Error("обработка выхода участника", append(attrs, "error", err)...)
		}
	case model.UpdateChatTitleChanged:
		if err := r.reg.ChatTitleChanged(ctx, upd.ChatID, upd.GetChat().Title); err != nil {
			r.log.Error("обработка смены названия чата", append(attrs, "error", err)...)
		}
	case model.UpdateBotStarted:
		r.onDialogStart(ctx, upd.GetUser(), attrs)
	case model.UpdateMessageCreated:
		r.onMessageCreated(ctx, upd, attrs)
	case model.UpdateMessageCallback:
		r.onCallback(ctx, upd, attrs)
	default:
		r.log.Debug("тип апдейта пока не обрабатывается", attrs...)
	}
}

func (r *Router) onBotAdded(ctx context.Context, upd model.Update, attrs []any) {
	var title string
	if upd.ChatProp != nil {
		title = upd.ChatProp.Title
	}
	status, err := r.reg.BotAdded(ctx, upd.ChatID, title)
	if err != nil {
		r.log.Error("обработка добавления бота", append(attrs, "error", err)...)
		return
	}

	var text string
	switch status {
	case housechat.StatusActive:
		text = ChatGreeting
	case housechat.StatusPending:
		text = ChatPending
	default:
		return
	}
	// Ошибка отправки не считается ошибкой обработки апдейта:
	// чат уже зарегистрирован, приветствие вторично.
	if _, err := r.out.SendChatMessage(ctx, upd.ChatID, text); err != nil {
		r.log.Error("отправка приветствия в чат", append(attrs, "error", err)...)
	}
}

func (r *Router) onUserAdded(ctx context.Context, upd model.Update, attrs []any) {
	if upd.User == nil || upd.User.IsBot {
		r.log.Debug("событие входа без пользователя или для бота, пропуск", attrs...)
		return
	}
	if err := r.reg.UserAdded(ctx, upd.ChatID, toUser(*upd.User)); err != nil {
		r.log.Error("обработка входа участника", append(attrs, "error", err)...)
	}
}

// onMessageCreated: в домовом чате сообщения жителей бот не обрабатывает.
// В личке сообщение получает сценарий текущего диалога (создание, правка,
// материал, рассылка); без диалога номер заявки («12», «№12») открывает её
// карточку, остальное ведёт в меню (раздел 7). Ошибка FSM (например,
// недоступен Redis) не блокирует пользователя: показываем меню.
func (r *Router) onMessageCreated(ctx context.Context, upd model.Update, attrs []any) {
	msg := upd.GetMessage()
	if msg.Recipient.ChatType != model.ChatTypeDialog || msg.Sender.IsBot {
		return
	}
	u := upd.GetUser()
	text, atts := msg.Body.Text, fromMaxAttachments(msg.Body.Attachments)

	if item, ok := parseCommand(text); ok {
		r.onCommand(ctx, u, item, append(attrs, "command", item))
		return
	}

	flow, err := r.dlg.Current(ctx, u.UserID)
	if err != nil {
		r.log.Error("чтение диалога", append(attrs, "error", err)...)
		r.onDialogStart(ctx, u, attrs)
		return
	}
	if r.handleDialog(ctx, u, flow, text, atts, attrs) {
		return
	}
	if id, ok := parseRequestNumber(text); ok && len(atts) == 0 {
		r.openInDM(ctx, u, id, attrs)
		return
	}
	r.onDialogStart(ctx, u, attrs)
}

// handleDialog передаёт сообщение сценарию текущего диалога.
// false — сообщение не обработано (диалога нет, он истёк или ошибка).
func (r *Router) handleDialog(ctx context.Context, u model.User, flow requests.Flow, text string, atts []request.Attachment, attrs []any) bool {
	var handled bool
	var err error
	switch flow {
	case requests.FlowCreate:
		var step requests.Step
		if step, handled, err = r.create.HandleMessage(ctx, u.UserID, text, atts); handled && err == nil {
			r.showStep(ctx, u, step, attrs)
		}
	case requests.FlowManage:
		var res requests.ManageResult
		if res, handled, err = r.manage.HandleMessage(ctx, u.UserID, text); handled && err == nil {
			r.showManage(ctx, u, res, attrs)
		}
	case requests.FlowMaterial:
		var res requests.MaterialResult
		if res, handled, err = r.mat.HandleMessage(ctx, toUser(u), text, atts); handled && err == nil {
			r.showMaterial(ctx, u, res, attrs)
		}
	case requests.FlowBroadcast:
		var res requests.BroadcastResult
		if res, handled, err = r.bc.HandleMessage(ctx, u.UserID, text); handled && err == nil {
			r.showBroadcast(ctx, u, res, attrs)
		}
	}
	if err != nil {
		r.log.Error("обработка сообщения в диалоге", append(attrs, "flow", int(flow), "error", err)...)
		return false
	}
	return handled
}

// onCommand — текстовая команда лички: то же, что кнопка меню. Как «В меню»,
// прерывает незавершённый диалог — иначе команда, набранная посреди
// черновика, стала бы его текстом.
func (r *Router) onCommand(ctx context.Context, u model.User, item string, attrs []any) {
	if err := r.dlg.Reset(ctx, u.UserID); err != nil {
		r.log.Error("сброс диалога", append(attrs, "error", err)...)
	}
	if item == menuMain {
		r.onDialogStart(ctx, u, attrs)
		return
	}
	// Без callback_id: ответа на нажатие нет, остальное — как у кнопки.
	r.onMenu(ctx, model.Callback{User: u}, item, attrs)
}

func (r *Router) onDialogStart(ctx context.Context, u model.User, attrs []any) {
	if u.UserID == 0 || u.IsBot {
		return
	}
	res, err := r.onb.Start(ctx, toUser(u))
	if err != nil {
		r.log.Error("обработка обращения в личке", append(attrs, "error", err)...)
		return
	}
	r.showOnboarding(ctx, u.UserID, res, attrs)
}

// onCallback обрабатывает нажатия кнопок в личке. Данным кнопки не доверяем:
// права и принадлежность чата проверяет use case.
func (r *Router) onCallback(ctx context.Context, upd model.Update, attrs []any) {
	cb := upd.GetCallback()
	userID := cb.User.UserID
	attrs = append(attrs, "callback_user_id", userID)

	action, param := parseCallback(cb.Payload)
	attrs = append(attrs, "action", action)

	if upd.GetMessage().Recipient.ChatType != model.ChatTypeDialog {
		// В домовом чате: голосование и «Открыть в личке».
		switch action {
		case cbVote:
			r.onVote(ctx, cb, param, attrs)
		case cbOpen:
			r.onOpenFromChat(ctx, cb, param, attrs)
		default:
			r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
		}
		return
	}

	switch action {
	case cbHome:
		chatID, err := strconv.ParseInt(param, 10, 64)
		if err != nil {
			r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
			return
		}
		res, err := r.onb.SelectChat(ctx, userID, chatID)
		if err != nil {
			r.log.Error("выбор домового чата", append(attrs, "error", err)...)
			r.answer(ctx, cb.CallbackID, NotifyError, attrs)
			return
		}
		notify := NotifyUnavailable
		if res.Outcome == onboarding.OutcomeReady {
			notify = NotifyHomeChosen
		}
		r.answer(ctx, cb.CallbackID, notify, attrs)
		r.showOnboarding(ctx, userID, res, attrs)

	case cbMenu:
		r.onMenu(ctx, cb, param, attrs)

	case cbCreate:
		r.onCreate(ctx, cb, param, attrs)

	case cbReq:
		r.onManage(ctx, cb, param, attrs)

	case cbOpen:
		id, err := strconv.ParseInt(param, 10, 64)
		if err != nil {
			r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
			return
		}
		r.answer(ctx, cb.CallbackID, NotifyOK, attrs)
		r.openInDM(ctx, cb.User, id, attrs)

	case cbMat:
		r.onMaterial(ctx, cb, param, attrs)

	case cbBc:
		r.onBroadcast(ctx, cb, param, attrs)

	case cbSet:
		r.onSettings(ctx, cb, param, attrs)

	case cbProblem:
		r.onProblem(ctx, cb, param, attrs)

	default:
		r.log.Debug("неизвестное действие кнопки", attrs...)
		r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
	}
}

func (r *Router) onMenu(ctx context.Context, cb model.Callback, item string, attrs []any) {
	userID := cb.User.UserID
	switch item {
	case menuHelp:
		r.answer(ctx, cb.CallbackID, BtnHelp, attrs)
		r.send(ctx, userID, helpText(), toMenuKeyboard(), attrs)
	case menuMain:
		// Как «Отмена»: незавершённый диалог прерывается, иначе следующее
		// сообщение ушло бы в старый черновик.
		if err := r.dlg.Reset(ctx, userID); err != nil {
			r.log.Error("сброс диалога", append(attrs, "error", err)...)
		}
		r.answer(ctx, cb.CallbackID, NotifyOK, attrs)
		r.onDialogStart(ctx, cb.User, attrs)
	case menuSwitch:
		res, err := r.onb.SwitchChat(ctx, userID)
		if err != nil {
			r.log.Error("смена домового чата", append(attrs, "error", err)...)
			r.answer(ctx, cb.CallbackID, NotifyError, attrs)
			return
		}
		r.answer(ctx, cb.CallbackID, BtnSwitchHome, attrs)
		r.showOnboarding(ctx, userID, res, attrs)
	case menuCreate:
		step, err := r.create.Begin(ctx, userID)
		r.answerStep(ctx, cb, step, err, attrs)
	case menuMine:
		res, err := r.manage.MyRequests(ctx, userID)
		r.answerManage(ctx, cb, res, err, attrs)
	case menuHouse:
		res, err := r.mat.HouseRequests(ctx, userID)
		r.answerMaterial(ctx, cb, res, err, attrs)
	case menuSettings:
		r.onSettings(ctx, cb, "", attrs)
	case menuProblem:
		res, err := r.prob.List(ctx, userID)
		r.answerProblem(ctx, cb, res, err, attrs)
	default:
		r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
	}
}

// onVote обрабатывает кнопку голосования "v:<confirm|refute>:<request_id>".
// Ответ — короткое уведомление нажавшему, счётчики в чате обновляет use case.
func (r *Router) onVote(ctx context.Context, cb model.Callback, param string, attrs []any) {
	rawValue, rawID := parseCallback(param)
	value, okValue := vote.Parse(rawValue)
	requestID, err := strconv.ParseInt(rawID, 10, 64)
	if !okValue || err != nil {
		r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
		return
	}
	attrs = append(attrs, "request_id", requestID)

	res, err := r.voting.Vote(ctx, requestID, cb.User.UserID, value)
	if err != nil {
		r.log.Error("голосование", append(attrs, "error", err)...)
		r.answer(ctx, cb.CallbackID, NotifyError, attrs)
		return
	}
	r.answer(ctx, cb.CallbackID, voteNotification(res), attrs)
}

func voteNotification(res voting.Result) string {
	switch res.Outcome {
	case voting.OutcomeAccepted, voting.OutcomeUnchanged:
		tt := typeTexts(res.Type)
		if res.Value == vote.Refute {
			return tt.VotedRefute
		}
		return tt.VotedConfirm
	case voting.OutcomeOwnRequest:
		return VoteOwnRequest
	case voting.OutcomeClosed:
		return VoteClosed
	case voting.OutcomeNotMember:
		return VoteNotMember
	case voting.OutcomeNotFound:
		return VoteNotFound
	}
	return NotifyUnavailable
}

// onCreate обрабатывает кнопки диалога создания заявки.
func (r *Router) onCreate(ctx context.Context, cb model.Callback, param string, attrs []any) {
	userID := cb.User.UserID
	step, arg := parseCallback(param)

	var res requests.Step
	var err error
	switch step {
	case createType:
		res, err = r.create.ChooseType(ctx, userID, requesttype.Code(arg))
	case createDone:
		res, err = r.create.FinishMedia(ctx, userID)
	case createAnon:
		if arg != anonYes && arg != anonNo {
			r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
			return
		}
		res, err = r.create.ChooseAnonymity(ctx, userID, arg == anonYes)
	case createEdit:
		res, err = r.create.Edit(ctx, userID)
	case createPublish:
		res, err = r.create.Publish(ctx, toUser(cb.User))
	case createCancel:
		res, err = r.create.Cancel(ctx, userID)
	default:
		r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
		return
	}
	r.answerStep(ctx, cb, res, err, attrs)
}

// answerStep отвечает на нажатие и показывает шаг создания заявки.
func (r *Router) answerStep(ctx context.Context, cb model.Callback, step requests.Step, err error, attrs []any) {
	if err != nil {
		r.log.Error("шаг создания заявки", append(attrs, "error", err)...)
		r.answer(ctx, cb.CallbackID, NotifyError, attrs)
		return
	}
	notify := NotifyOK
	if step.Problem != nil {
		notify = NotifyUnavailable
	}
	r.answer(ctx, cb.CallbackID, notify, attrs)
	r.showStep(ctx, cb.User, step, attrs)
}

// showStep отображает шаг создания заявки. Итоговые шаги (опубликовано,
// отменено, устарело, отказ) завершаются показом меню.
func (r *Router) showStep(ctx context.Context, u model.User, step requests.Step, attrs []any) {
	userID := u.UserID
	d := step.Draft
	lim := step.Limits

	var text string
	var kb *model.Keyboard
	toMenu := false
	switch step.Kind {
	case requests.StepNoChat:
		r.onDialogStart(ctx, u, attrs)
		return
	case requests.StepChooseType:
		text, kb = CreateChooseType, createTypeKeyboard(step.Types)
	case requests.StepEnterText:
		text = fmt.Sprintf(CreateEnterText, typeTexts(d.Type).Name, lim.MaxBodyLen)
		kb = createCancelKeyboard()
	case requests.StepAddMedia:
		text = fmt.Sprintf(CreateAddMedia, len(d.Attachments), lim.MaxAttachments)
		kb = createMediaKeyboard()
	case requests.StepChooseAnonymity:
		text, kb = CreateChooseAnonymity, createAnonymityKeyboard()
	case requests.StepConfirm:
		authorLine := CreateAuthorShown
		if d.Anonymous {
			authorLine = CreateAuthorAnonymous
		}
		text = fmt.Sprintf(CreateConfirm, typeTexts(d.Type).Name, authorLine, len(d.Attachments), d.Body)
		kb = createConfirmKeyboard()
	case requests.StepPublished:
		text = fmt.Sprintf(CreatePublished, step.Request.ID)
		if !step.ChatSynced {
			text = fmt.Sprintf(CreatePublishFailed, step.Request.ID)
		}
		toMenu = true
	case requests.StepCancelled:
		text, toMenu = CreateCancelled, true
	case requests.StepExpired:
		text, toMenu = CreateExpired, true
	case requests.StepLimitReached:
		text, toMenu = fmt.Sprintf(CreateLimitReached, typeTexts(d.Type).Name, lim.DailyLimit), true
	case requests.StepRatingTooLow:
		text, toMenu = CreateRatingTooLow, true
	default:
		return
	}
	if p := problemText(step.Problem, lim); p != "" {
		text = p + "\n\n" + text
	}
	r.send(ctx, userID, text, kb, attrs)
	if toMenu {
		r.onDialogStart(ctx, u, attrs)
	}
}

// problemText — пояснение, почему шаг показан повторно.
func problemText(problem error, lim request.CreatePolicy) string {
	switch {
	case problem == nil:
		return ""
	case errors.Is(problem, request.ErrEmptyBody):
		return ProblemEmptyBody
	case errors.Is(problem, request.ErrBodyTooLong):
		return fmt.Sprintf(ProblemBodyTooLong, lim.MaxBodyLen)
	case errors.Is(problem, request.ErrDailyLimit):
		return fmt.Sprintf(ProblemTypeLimit, lim.DailyLimit)
	case errors.Is(problem, comment.ErrNoMedia):
		return ProblemNoMedia
	case errors.Is(problem, request.ErrTooManyMedia):
		return fmt.Sprintf(ProblemTooManyMedia, lim.MaxAttachments)
	default:
		return ProblemUnexpected
	}
}

// onManage обрабатывает кнопки управления своей заявкой "req:<действие>:<id>".
func (r *Router) onManage(ctx context.Context, cb model.Callback, param string, attrs []any) {
	userID := cb.User.UserID
	action, rawID := parseCallback(param)
	if action == reqCancel {
		res, err := r.manage.Cancel(ctx, userID)
		r.answerManage(ctx, cb, res, err, attrs)
		return
	}
	requestID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
		return
	}
	attrs = append(attrs, "request_id", requestID)

	var res requests.ManageResult
	switch action {
	case reqOpen:
		res, err = r.manage.Open(ctx, requestID, userID)
	case reqEdit:
		res, err = r.manage.StartEdit(ctx, requestID, userID)
	case reqClose:
		res, err = r.manage.StartClose(ctx, requestID, userID)
	case reqDelete:
		res, err = r.manage.AskDelete(ctx, requestID, userID)
	case reqDeleteConfirm:
		res, err = r.manage.Delete(ctx, requestID, userID)
	default:
		r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
		return
	}
	r.answerManage(ctx, cb, res, err, attrs)
}

func (r *Router) answerManage(ctx context.Context, cb model.Callback, res requests.ManageResult, err error, attrs []any) {
	if err != nil {
		r.log.Error("управление заявкой", append(attrs, "error", err)...)
		r.answer(ctx, cb.CallbackID, NotifyError, attrs)
		return
	}
	notify := NotifyOK
	if res.Kind == requests.ManageNotFound || res.Kind == requests.ManageNotActive {
		notify = NotifyUnavailable
	}
	r.answer(ctx, cb.CallbackID, notify, attrs)
	r.showManage(ctx, cb.User, res, attrs)
}

// showManage отображает результат управления заявкой. Итоговые действия
// завершаются показом меню.
func (r *Router) showManage(ctx context.Context, u model.User, res requests.ManageResult, attrs []any) {
	req := res.Card.Request
	var text string
	var kb *model.Keyboard
	toMenu := false
	switch res.Kind {
	case requests.ManageNoChat:
		r.onDialogStart(ctx, u, attrs)
		return
	case requests.ManageList:
		if len(res.Requests) == 0 {
			text, toMenu = fmt.Sprintf(MyRequestsEmpty, chatLabel(res.Chat)), true
		} else {
			text, kb = fmt.Sprintf(MyRequestsTitle, chatLabel(res.Chat)), myRequestsKeyboard(res.Requests)
		}
	case requests.ManageCard:
		text, kb = authorCardText(res.Card), authorCardKeyboard(req, res.Card.Materials)
	case requests.ManageAskResult:
		text, kb = fmt.Sprintf(AskResult, req.ID), manageCancelKeyboard()
	case requests.ManageAskBody:
		text, kb = fmt.Sprintf(AskBody, req.ID), manageCancelKeyboard()
	case requests.ManageConfirmDelete:
		text, kb = fmt.Sprintf(ConfirmDelete, req.ID), confirmDeleteKeyboard(req.ID)
	case requests.ManageClosed:
		text, toMenu = withSync(fmt.Sprintf(RequestClosed, req.ID), res.ChatSynced), true
	case requests.ManageEdited:
		text, toMenu = withSync(fmt.Sprintf(RequestEdited, req.ID), res.ChatSynced), true
	case requests.ManageDeleted:
		text, toMenu = withSync(fmt.Sprintf(RequestDeleted, req.ID), res.ChatSynced), true
	case requests.ManageNotFound:
		text, toMenu = RequestNotFound, true
	case requests.ManageNotActive:
		text, toMenu = RequestNotActive, true
	case requests.ManageCancelled:
		text, toMenu = ManageCancelled, true
	default:
		return
	}
	if p := problemText(res.Problem, res.Limits); p != "" {
		text = p + "\n\n" + text
	}
	r.send(ctx, u.UserID, text, kb, attrs)
	if toMenu {
		r.onDialogStart(ctx, u, attrs)
	}
}

func withSync(text string, synced bool) string {
	if synced {
		return text
	}
	return text + "\n\n" + ChatNotSynced
}

// authorCardText — карточка заявки для автора в личке. Автор видит свою
// заявку целиком, в том числе анонимную.
func authorCardText(card usecase.RequestCard) string {
	req := card.Request
	tt := typeTexts(req.Type)
	text := fmt.Sprintf(AuthorCard, req.ID, tt.Name, StatusNames[req.Status], req.Body,
		tt.Confirm, card.Confirms, tt.Refute, card.Refutes)
	if req.Status == request.StatusClosed && req.ResultText != "" {
		text += "\n\n" + fmt.Sprintf(AuthorCardResult, req.ResultText)
	}
	return text
}

// showOnboarding отображает результат сценария первого сообщения.
func (r *Router) showOnboarding(ctx context.Context, userID int64, res onboarding.Result, attrs []any) {
	switch res.Outcome {
	case onboarding.OutcomeNotRegistered:
		r.send(ctx, userID, DMNotRegistered, nil, attrs)
	case onboarding.OutcomeChooseChat:
		r.send(ctx, userID, DMChooseChat, chooseChatKeyboard(res.Chats), attrs)
	case onboarding.OutcomeReady:
		tmpl := DMMenu
		if res.Greet {
			tmpl = DMGreeting
		}
		r.send(ctx, userID, fmt.Sprintf(tmpl, chatLabel(res.Chat)), menuKeyboard(res.CanSwitch()), attrs)
	}
}

func (r *Router) send(ctx context.Context, userID int64, text string, kb *model.Keyboard, attrs []any) {
	if err := r.out.SendUserMessage(ctx, userID, text, kb); err != nil {
		r.log.Error("отправка сообщения в личку", append(attrs, "error", err)...)
	}
}

func (r *Router) answer(ctx context.Context, callbackID, notification string, attrs []any) {
	if callbackID == "" {
		return
	}
	if err := r.out.AnswerCallback(ctx, callbackID, notification); err != nil {
		r.log.Error("ответ на нажатие кнопки", append(attrs, "error", err)...)
	}
}

func toUser(u model.User) membership.User {
	return membership.User{ID: u.UserID, Nickname: nickname(u.Name, u.Username, u.FirstName)}
}
