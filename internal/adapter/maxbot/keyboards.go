package maxbot

import (
	"fmt"
	"strconv"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"

	"maxhouse/internal/domain/broadcast"
	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/problem"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/domain/vote"
)

// menuKeyboard — главное меню лички.
// Кнопка «Сменить дом» показывается, только если чатов несколько.
func menuKeyboard(canSwitch bool) *model.Keyboard {
	kb := model.NewKeyboard()
	kb.AddRow().AddCallBack(BtnCreateRequest, callbackData(cbMenu, menuCreate))
	kb.AddRow().AddCallBack(BtnReportProblem, callbackData(cbMenu, menuProblem))
	kb.AddRow().
		AddCallBack(BtnMyRequests, callbackData(cbMenu, menuMine)).
		AddCallBack(BtnHouseRequests, callbackData(cbMenu, menuHouse))
	kb.AddRow().
		AddCallBack(BtnSettings, callbackData(cbMenu, menuSettings)).
		AddCallBack(BtnHelp, callbackData(cbMenu, menuHelp))
	if canSwitch {
		kb.AddRow().AddCallBack(BtnSwitchHome, callbackData(cbMenu, menuSwitch))
	}
	return kb
}

// problemKeyboard — выбор проблемы, названия из ProblemNames.
func problemKeyboard(codes []problem.Code) *model.Keyboard {
	kb := model.NewKeyboard()
	for _, c := range codes {
		kb.AddRow().AddCallBack(ProblemNames[c], callbackData(cbProblem, string(c)))
	}
	return toMenuRow(kb)
}

// chooseChatKeyboard — выбор домового чата, по кнопке на чат.
func chooseChatKeyboard(chats []housechat.Chat) *model.Keyboard {
	kb := model.NewKeyboard()
	for _, c := range chats {
		kb.AddRow().AddCallBack(chatLabel(c), homeCallback(c.ID))
	}
	return kb
}

func chatLabel(c housechat.Chat) string {
	if c.Title != "" {
		return c.Title
	}
	return fmt.Sprintf(ChatUntitled, c.ID)
}

// toMenuRow добавляет строку «В меню» (callback menu:main).
func toMenuRow(kb *model.Keyboard) *model.Keyboard {
	kb.AddRow().AddCallBack(BtnToMenu, callbackData(cbMenu, menuMain))
	return kb
}

// toMenuKeyboard — клавиатура из одной кнопки «В меню».
func toMenuKeyboard() *model.Keyboard {
	return toMenuRow(model.NewKeyboard())
}

func cancelRow(kb *model.Keyboard) {
	kb.AddRow().AddCallBack(BtnCancel, createCallback(createCancel))
}

// createTypeKeyboard — выбор типа заявки, названия из RequestTypeTexts.
func createTypeKeyboard(types []requesttype.Type) *model.Keyboard {
	kb := model.NewKeyboard()
	for _, t := range types {
		kb.AddRow().AddCallBack(typeTexts(t.Code).Name, createCallback(createType, string(t.Code)))
	}
	cancelRow(kb)
	return kb
}

// createCancelKeyboard — шаг ввода текста: только «Отмена».
func createCancelKeyboard() *model.Keyboard {
	kb := model.NewKeyboard()
	cancelRow(kb)
	return kb
}

func createMediaKeyboard() *model.Keyboard {
	kb := model.NewKeyboard()
	kb.AddRow().AddCallBack(BtnDone, createCallback(createDone))
	cancelRow(kb)
	return kb
}

func createAnonymityKeyboard() *model.Keyboard {
	kb := model.NewKeyboard()
	kb.AddRow().
		AddCallBack(BtnShowAuthor, createCallback(createAnon, anonNo)).
		AddCallBack(BtnAnonymous, createCallback(createAnon, anonYes))
	cancelRow(kb)
	return kb
}

func createConfirmKeyboard() *model.Keyboard {
	kb := model.NewKeyboard()
	kb.AddRow().AddCallBack(BtnPublish, createCallback(createPublish))
	kb.AddRow().AddCallBack(BtnEditText, createCallback(createEdit))
	cancelRow(kb)
	return kb
}

// chatCardKeyboard — кнопки под карточкой в чате: голосование (пока оно идёт,
// подписи по типу заявки) и «Открыть в личке» для материалов (раздел 8.4).
// В payload — только действие и id заявки.
func chatCardKeyboard(r request.Request) *model.Keyboard {
	kb := model.NewKeyboard()
	if r.Status == request.StatusOpen {
		tt := typeTexts(r.Type)
		kb.AddRow().
			AddCallBack(tt.Confirm, voteCallback(vote.Confirm, r.ID)).
			AddCallBack(tt.Refute, voteCallback(vote.Refute, r.ID))
	}
	kb.AddRow().AddCallBack(BtnOpenInDM, openCallback(r.ID))
	return kb
}

// requestListKeyboard — список заявок дома, по кнопке на заявку.
func requestListKeyboard(list []request.Request) *model.Keyboard {
	kb := model.NewKeyboard()
	for _, r := range list {
		label := fmt.Sprintf(MyRequestItem, r.ID, typeTexts(r.Type).Name, StatusNames[r.Status])
		kb.AddRow().AddCallBack(label, openCallback(r.ID))
	}
	return toMenuRow(kb)
}

// neighborCardKeyboard — действия соседа (если материалы ещё принимаются)
// и «В меню».
func neighborCardKeyboard(requestID int64, canContribute bool) *model.Keyboard {
	kb := model.NewKeyboard()
	if canContribute {
		kb.AddRow().AddCallBack(BtnComment, matCallback(matComment, requestID))
		kb.AddRow().AddCallBack(BtnEvidence, matCallback(matEvidence, requestID))
	}
	return toMenuRow(kb)
}

func matCancelKeyboard() *model.Keyboard {
	kb := model.NewKeyboard()
	kb.AddRow().AddCallBack(BtnCancel, callbackData(cbMat, matCancel))
	return kb
}

func openKeyboard(payload string) *model.Keyboard {
	kb := model.NewKeyboard()
	kb.AddRow().AddCallBack(BtnOpen, payload)
	return kb
}

// materialsPageKeyboard — навигация по материалам: «Показать ещё»
// (если next > 0) и «В меню».
func materialsPageKeyboard(requestID int64, next int) *model.Keyboard {
	kb := model.NewKeyboard()
	if next > 0 {
		kb.AddRow().AddCallBack(BtnMore, matListCallback(requestID, next))
	}
	return toMenuRow(kb)
}

// audienceKeyboard — аудитории рассылки, подписи по типу заявки.
func audienceKeyboard(r request.Request) *model.Keyboard {
	tt := typeTexts(r.Type)
	labels := map[broadcast.Audience]string{
		broadcast.AudienceAll:     tt.AudienceAll,
		broadcast.AudienceConfirm: tt.AudienceConfirm,
		broadcast.AudienceRefute:  tt.AudienceRefute,
	}
	kb := model.NewKeyboard()
	for _, a := range broadcast.Audiences {
		kb.AddRow().AddCallBack(labels[a], bcAudCallback(r.ID, string(a)))
	}
	kb.AddRow().AddCallBack(BtnCancel, callbackData(cbBc, bcCancel))
	return kb
}

func audienceLabel(r request.Request, a broadcast.Audience) string {
	tt := typeTexts(r.Type)
	switch a {
	case broadcast.AudienceConfirm:
		return tt.AudienceConfirm
	case broadcast.AudienceRefute:
		return tt.AudienceRefute
	}
	return tt.AudienceAll
}

func bcCancelKeyboard() *model.Keyboard {
	kb := model.NewKeyboard()
	kb.AddRow().AddCallBack(BtnCancel, callbackData(cbBc, bcCancel))
	return kb
}

func bcConfirmKeyboard() *model.Keyboard {
	kb := model.NewKeyboard()
	kb.AddRow().
		AddCallBack(BtnSend, callbackData(cbBc, bcSend)).
		AddCallBack(BtnCancel, callbackData(cbBc, bcCancel))
	return kb
}

// settingsKeyboard — переключатели: подпись кнопки — действие,
// противоположное текущему состоянию.
func settingsKeyboard(forwardOn, notifyMaterialsOn bool) *model.Keyboard {
	forward := BtnForwardOn
	if forwardOn {
		forward = BtnForwardOff
	}
	materials := BtnNotifyMatOn
	if notifyMaterialsOn {
		materials = BtnNotifyMatOff
	}
	kb := model.NewKeyboard()
	kb.AddRow().AddCallBack(forward, callbackData(cbSet, setForward))
	kb.AddRow().AddCallBack(materials, callbackData(cbSet, setMaterials))
	return toMenuRow(kb)
}

// myRequestsKeyboard — список своих заявок, по кнопке на заявку.
func myRequestsKeyboard(list []request.Request) *model.Keyboard {
	kb := model.NewKeyboard()
	for _, r := range list {
		label := fmt.Sprintf(MyRequestItem, r.ID, typeTexts(r.Type).Name, StatusNames[r.Status])
		kb.AddRow().AddCallBack(label, reqCallback(reqOpen, r.ID))
	}
	return toMenuRow(kb)
}

// authorCardKeyboard — действия автора. Материалы и рассылка доступны
// всегда; правка, закрытие и удаление — только у open/in_progress.
func authorCardKeyboard(r request.Request, materials int) *model.Keyboard {
	kb := model.NewKeyboard()
	kb.AddRow().AddCallBack(fmt.Sprintf(BtnMaterials, materials), matListCallback(r.ID, 0))
	kb.AddRow().AddCallBack(BtnBroadcast, callbackData(cbBc, bcStart+":"+strconv.FormatInt(r.ID, 10)))
	if r.IsActive() {
		kb.AddRow().
			AddCallBack(BtnEditRequest, reqCallback(reqEdit, r.ID)).
			AddCallBack(BtnCloseRequest, reqCallback(reqClose, r.ID))
		kb.AddRow().AddCallBack(BtnDeleteRequest, reqCallback(reqDelete, r.ID))
	}
	return toMenuRow(kb)
}

func manageCancelKeyboard() *model.Keyboard {
	kb := model.NewKeyboard()
	kb.AddRow().AddCallBack(BtnCancel, callbackData(cbReq, reqCancel))
	return kb
}

func confirmDeleteKeyboard(requestID int64) *model.Keyboard {
	kb := model.NewKeyboard()
	kb.AddRow().
		AddCallBack(BtnConfirmDelete, reqCallback(reqDeleteConfirm, requestID)).
		AddCallBack(BtnCancel, callbackData(cbReq, reqCancel))
	return kb
}
