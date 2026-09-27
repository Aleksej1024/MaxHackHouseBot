package maxbot

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"

	"maxhouse/internal/domain/broadcast"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/usecase"
	"maxhouse/internal/usecase/requests"
	"maxhouse/internal/usecase/settings"
)

// Сценарии лички этапа 6

// Dialogs — какой сценарий ведёт диалог пользователя.
type Dialogs interface {
	Current(ctx context.Context, userID int64) (requests.Flow, error)
	Reset(ctx context.Context, userID int64) error
}

// MaterialsScenario — просмотр заявок жителями, комментарии и доказательства.
type MaterialsScenario interface {
	HouseRequests(ctx context.Context, userID int64) (requests.MaterialResult, error)
	View(ctx context.Context, requestID, userID int64) (requests.MaterialResult, error)
	StartComment(ctx context.Context, requestID, userID int64) (requests.MaterialResult, error)
	StartEvidence(ctx context.Context, requestID, userID int64) (requests.MaterialResult, error)
	Cancel(ctx context.Context, userID int64) (requests.MaterialResult, error)
	HandleMessage(ctx context.Context, u membership.User, text string, atts []request.Attachment) (requests.MaterialResult, bool, error)
	ListMaterials(ctx context.Context, requestID, userID int64, offset int) (requests.MaterialResult, error)
}

// BroadcastScenario — рассылка автора по заявке.
type BroadcastScenario interface {
	Start(ctx context.Context, requestID, userID int64) (requests.BroadcastResult, error)
	ChooseAudience(ctx context.Context, requestID, userID int64, aud broadcast.Audience) (requests.BroadcastResult, error)
	HandleMessage(ctx context.Context, userID int64, text string) (requests.BroadcastResult, bool, error)
	Send(ctx context.Context, userID int64) (requests.BroadcastResult, error)
	Cancel(ctx context.Context, userID int64) (requests.BroadcastResult, error)
}

// SettingsScenario — настройки жителя.
type SettingsScenario interface {
	Get(ctx context.Context, userID int64) (settings.Result, error)
	ToggleForward(ctx context.Context, userID int64) (settings.Result, error)
	ToggleNotifyMaterials(ctx context.Context, userID int64) (settings.Result, error)
}

var requestNumberRe = regexp.MustCompile(`^\s*№?\s*(\d{1,18})\s*$`)

// parseRequestNumber распознаёт номер заявки в сообщении: «12» или «№12».
func parseRequestNumber(text string) (int64, bool) {
	m := requestNumberRe.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	return id, err == nil && id > 0
}

// openInDM показывает карточку заявки в личке (по номеру или кнопке).
func (r *Router) openInDM(ctx context.Context, u model.User, requestID int64, attrs []any) {
	res, err := r.mat.View(ctx, requestID, u.UserID)
	if err != nil {
		r.log.Error("открытие заявки", append(attrs, "request_id", requestID, "error", err)...)
		return
	}
	r.showMaterial(ctx, u, res, attrs)
}

// onOpenFromChat — «Открыть в личке» под карточкой в чате. Бот может
// написать в личку только тому, кто начал с ним диалог (раздел 8.6).
func (r *Router) onOpenFromChat(ctx context.Context, cb model.Callback, param string, attrs []any) {
	requestID, err := strconv.ParseInt(param, 10, 64)
	if err != nil {
		r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
		return
	}
	attrs = append(attrs, "request_id", requestID)
	res, err := r.mat.View(ctx, requestID, cb.User.UserID)
	if err != nil {
		r.log.Error("открытие заявки из чата", append(attrs, "error", err)...)
		r.answer(ctx, cb.CallbackID, NotifyError, attrs)
		return
	}
	text, kb, ok := cardView(res)
	if !ok {
		r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
		return
	}
	if err := r.out.SendUserMessage(ctx, cb.User.UserID, text, kb); err != nil {
		r.log.Debug("личка недоступна", append(attrs, "error", err)...)
		r.answer(ctx, cb.CallbackID, NotifyStartDialog, attrs)
		return
	}
	r.answer(ctx, cb.CallbackID, NotifyCardSent, attrs)
}

// cardView — текст и кнопки карточки заявки для автора или соседа.
func cardView(res requests.MaterialResult) (string, *model.Keyboard, bool) {
	req := res.Card.Request
	switch res.Kind {
	case requests.MatAuthorCard:
		return authorCardText(res.Card), authorCardKeyboard(req, res.Card.Materials), true
	case requests.MatNeighborCard:
		return neighborCardText(res.Card), neighborCardKeyboard(req.ID, res.CanContribute), true
	}
	return "", nil, false
}

// neighborCardText — карточка для соседа: автор анонимной заявки скрыт,
// материалы других жителей не показываются.
func neighborCardText(card usecase.RequestCard) string {
	req := card.Request
	tt := typeTexts(req.Type)
	author := CardAuthorAnonymous
	if !req.IsAnonymous && card.AuthorNickname != "" {
		author = fmt.Sprintf(CardAuthor, card.AuthorNickname)
	}
	text := fmt.Sprintf(NeighborCard, req.ID, tt.Name, StatusNames[req.Status], req.Body,
		author, tt.Confirm, card.Confirms, tt.Refute, card.Refutes)
	if status := cardStatus(req); status != "" {
		text += "\n\n" + status
	}
	return text
}

// --- Материалы ---

func (r *Router) onMaterial(ctx context.Context, cb model.Callback, param string, attrs []any) {
	userID := cb.User.UserID
	action, rest := parseCallback(param)
	if action == matCancel {
		res, err := r.mat.Cancel(ctx, userID)
		r.answerMaterial(ctx, cb, res, err, attrs)
		return
	}
	rawID, rawOffset := parseCallback(rest)
	requestID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
		return
	}
	attrs = append(attrs, "request_id", requestID)

	var res requests.MaterialResult
	switch action {
	case matComment:
		res, err = r.mat.StartComment(ctx, requestID, userID)
	case matEvidence:
		res, err = r.mat.StartEvidence(ctx, requestID, userID)
	case matList:
		offset, convErr := strconv.Atoi(rawOffset)
		if convErr != nil || offset < 0 {
			r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
			return
		}
		res, err = r.mat.ListMaterials(ctx, requestID, userID, offset)
	default:
		r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
		return
	}
	r.answerMaterial(ctx, cb, res, err, attrs)
}

func (r *Router) answerMaterial(ctx context.Context, cb model.Callback, res requests.MaterialResult, err error, attrs []any) {
	if err != nil {
		r.log.Error("материалы заявки", append(attrs, "error", err)...)
		r.answer(ctx, cb.CallbackID, NotifyError, attrs)
		return
	}
	notify := NotifyOK
	switch res.Kind {
	case requests.MatNotFound, requests.MatNotActive, requests.MatOwnRequest:
		notify = NotifyUnavailable
	}
	r.answer(ctx, cb.CallbackID, notify, attrs)
	r.showMaterial(ctx, cb.User, res, attrs)
}

// showMaterial отображает результат сценария материалов.
func (r *Router) showMaterial(ctx context.Context, u model.User, res requests.MaterialResult, attrs []any) {
	req := res.Card.Request
	var text string
	var kb *model.Keyboard
	toMenu := false
	switch res.Kind {
	case requests.MatNoChat:
		r.onDialogStart(ctx, u, attrs)
		return
	case requests.MatHouseList:
		if len(res.Requests) == 0 {
			text, toMenu = fmt.Sprintf(HouseRequestsEmpty, chatLabel(res.Chat)), true
		} else {
			text, kb = fmt.Sprintf(HouseRequestsTitle, chatLabel(res.Chat)), requestListKeyboard(res.Requests)
		}
	case requests.MatAuthorCard, requests.MatNeighborCard:
		text, kb, _ = cardView(res)
	case requests.MatAskComment:
		text, kb = fmt.Sprintf(CommentHint, req.ID), matCancelKeyboard()
	case requests.MatAskEvidence:
		text, kb = fmt.Sprintf(EvidenceHint, req.ID), matCancelKeyboard()
	case requests.MatAdded:
		text, toMenu = fmt.Sprintf(MaterialSent, req.ID), true
	case requests.MatMaterials:
		r.showMaterialsPage(ctx, u.UserID, res, attrs)
		return
	case requests.MatNotFound:
		text, toMenu = RequestNotFound, true
	case requests.MatNotActive:
		text, toMenu = MaterialNotActive, true
	case requests.MatOwnRequest:
		text, kb = MaterialOwnRequest, toMenuKeyboard()
	case requests.MatCancelled:
		text, toMenu = MaterialCancelled, true
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

// showMaterialsPage — материалы автору: каждый отдельным сообщением
// с вложениями, затем навигация «Показать ещё».
func (r *Router) showMaterialsPage(ctx context.Context, userID int64, res requests.MaterialResult, attrs []any) {
	req := res.Card.Request
	if res.Total == 0 || len(res.Comments) == 0 {
		r.send(ctx, userID, fmt.Sprintf(MaterialsEmpty, req.ID), toMenuKeyboard(), attrs)
		return
	}
	for _, c := range res.Comments {
		text := fmt.Sprintf(MaterialItem, MaterialKindNames[c.Kind], c.AuthorNickname,
			c.CreatedAt.In(r.loc).Format(TimeLayout), c.Body)
		if err := r.out.SendUserMedia(ctx, userID, text, c.Attachments, nil); err != nil {
			r.log.Error("отправка материала автору", append(attrs, "comment_id", c.ID, "error", err)...)
		}
	}
	page := fmt.Sprintf(MaterialsPage, res.Offset+1, res.Offset+len(res.Comments), res.Total)
	r.send(ctx, userID, page, materialsPageKeyboard(req.ID, res.NextOffset()), attrs)
}

// --- Рассылка ---

func (r *Router) onBroadcast(ctx context.Context, cb model.Callback, param string, attrs []any) {
	userID := cb.User.UserID
	action, rest := parseCallback(param)

	var res requests.BroadcastResult
	var err error
	switch action {
	case bcSend:
		res, err = r.bc.Send(ctx, userID)
	case bcCancel:
		res, err = r.bc.Cancel(ctx, userID)
	case bcStart, bcAud:
		rawID, rawAud := parseCallback(rest)
		requestID, convErr := strconv.ParseInt(rawID, 10, 64)
		if convErr != nil {
			r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
			return
		}
		attrs = append(attrs, "request_id", requestID)
		if action == bcStart {
			res, err = r.bc.Start(ctx, requestID, userID)
			break
		}
		aud, ok := broadcast.ParseAudience(rawAud)
		if !ok {
			r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
			return
		}
		res, err = r.bc.ChooseAudience(ctx, requestID, userID, aud)
	default:
		r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
		return
	}
	if err != nil {
		r.log.Error("рассылка", append(attrs, "error", err)...)
		r.answer(ctx, cb.CallbackID, NotifyError, attrs)
		return
	}
	notify := NotifyOK
	if res.Kind == requests.BcNotFound || res.Kind == requests.BcCooldown {
		notify = NotifyUnavailable
	}
	r.answer(ctx, cb.CallbackID, notify, attrs)
	r.showBroadcast(ctx, cb.User, res, attrs)
}

// showBroadcast отображает шаг рассылки.
func (r *Router) showBroadcast(ctx context.Context, u model.User, res requests.BroadcastResult, attrs []any) {
	req := res.Request
	var text string
	var kb *model.Keyboard
	toMenu := false
	switch res.Kind {
	case requests.BcChooseAudience:
		text, kb = fmt.Sprintf(BroadcastChooseAudience, req.ID), audienceKeyboard(req)
	case requests.BcAskText:
		text, kb = fmt.Sprintf(BroadcastAskText, audienceLabel(req, res.Audience)), bcCancelKeyboard()
	case requests.BcConfirm:
		text = fmt.Sprintf(BroadcastConfirm, req.ID, audienceLabel(req, res.Audience), res.Recipients, res.Text)
		kb = bcConfirmKeyboard()
	case requests.BcSent:
		text = fmt.Sprintf(BroadcastSent, req.ID, res.Delivery.Delivered, res.Recipients)
		if res.Delivery.Unavailable+res.Delivery.Failed > 0 {
			text += "\n\n" + BroadcastUndelivered
		}
		toMenu = true
	case requests.BcCooldown:
		text = fmt.Sprintf(BroadcastCooldown, req.ID, res.NextAt.In(r.loc).Format(TimeLayout))
		toMenu = true
	case requests.BcNotFound:
		text, toMenu = RequestNotFound, true
	case requests.BcCancelled:
		text, toMenu = BroadcastCancelled, true
	case requests.BcExpired:
		text, toMenu = BroadcastExpired, true
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

// --- Настройки ---

// onSettings — «Настройки» из меню (param пуст), «set:forward», «set:materials».
func (r *Router) onSettings(ctx context.Context, cb model.Callback, param string, attrs []any) {
	var res settings.Result
	var err error
	switch param {
	case "":
		res, err = r.set.Get(ctx, cb.User.UserID)
	case setForward:
		res, err = r.set.ToggleForward(ctx, cb.User.UserID)
	case setMaterials:
		res, err = r.set.ToggleNotifyMaterials(ctx, cb.User.UserID)
	default:
		r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
		return
	}
	if err != nil {
		r.log.Error("настройки", append(attrs, "error", err)...)
		r.answer(ctx, cb.CallbackID, NotifyError, attrs)
		return
	}
	r.answer(ctx, cb.CallbackID, NotifyOK, attrs)
	if res.NoChat {
		r.onDialogStart(ctx, cb.User, attrs)
		return
	}
	create := CreateLimited
	if res.CanCreate {
		create = CreateAllowed
	}
	text := fmt.Sprintf(SettingsText, chatLabel(res.Chat), onOff(res.ForwardToDM), onOff(res.NotifyMaterials), create)
	r.send(ctx, cb.User.UserID, text, settingsKeyboard(res.ForwardToDM, res.NotifyMaterials), attrs)
}

func onOff(on bool) string {
	if on {
		return SettingOn
	}
	return SettingOff
}
