package maxbot

import (
	"strconv"
	"strings"

	"maxhouse/internal/domain/vote"
)

// Коды callback_data: только код действия и id сущности,
// без подписей и данных пользователя. Права проверяются на сервере.
const (
	// cbHome — выбор домового чата: "home:<chat_id>".
	cbHome = "home"
	// cbMenu — пункт меню лички: "menu:<пункт>".
	cbMenu = "menu"
	// cbCreate — шаг создания заявки: "new:<шаг>[:<параметр>]".
	cbCreate = "new"
	// cbVote — голос в домовом чате: "v:<confirm|refute>:<request_id>".
	cbVote = "v"
	// cbReq — управление своей заявкой в личке: "req:<действие>[:<request_id>]".
	cbReq = "req"
	// cbOpen — открыть заявку в личке (из чата или из списка): "open:<request_id>".
	cbOpen = "open"
	// cbMat — материалы: "mat:<comment|evidence>:<id>", "mat:list:<id>:<offset>", "mat:cancel".
	cbMat = "mat"
	// cbBc — рассылка: "bc:start:<id>", "bc:aud:<id>:<аудитория>", "bc:send", "bc:cancel".
	cbBc = "bc"
	// cbSet — настройки: "set:forward", "set:materials".
	cbSet = "set"
	// cbProblem — сообщить о проблеме: "pr:<код проблемы>".
	cbProblem = "pr"
)

// Действия с материалами и рассылкой.
const (
	matComment  = "comment"
	matEvidence = "evidence"
	matList     = "list"
	matCancel   = "cancel"

	bcStart  = "start"
	bcAud    = "aud"
	bcSend   = "send"
	bcCancel = "cancel"

	setForward   = "forward"
	setMaterials = "materials"
)

func openCallback(requestID int64) string {
	return callbackData(cbOpen, strconv.FormatInt(requestID, 10))
}

func matCallback(action string, requestID int64) string {
	return callbackData(cbMat, action+":"+strconv.FormatInt(requestID, 10))
}

func matListCallback(requestID int64, offset int) string {
	return callbackData(cbMat, matList+":"+strconv.FormatInt(requestID, 10)+":"+strconv.Itoa(offset))
}

func bcAudCallback(requestID int64, aud string) string {
	return callbackData(cbBc, bcAud+":"+strconv.FormatInt(requestID, 10)+":"+aud)
}

// Действия со своей заявкой (параметр cbReq).
const (
	reqOpen          = "open"
	reqEdit          = "edit"
	reqClose         = "close"
	reqDelete        = "delete"
	reqDeleteConfirm = "delete_yes"
	reqCancel        = "cancel"
)

func reqCallback(action string, requestID int64) string {
	return callbackData(cbReq, action+":"+strconv.FormatInt(requestID, 10))
}

// Шаги создания заявки (параметр cbCreate).
const (
	createType    = "type" // new:type:<код типа>
	createDone    = "done"
	createAnon    = "anon" // new:anon:yes | new:anon:no
	createPublish = "publish"
	createEdit    = "edit"
	createCancel  = "cancel"

	anonYes = "yes"
	anonNo  = "no"
)

// Пункты меню лички (параметр cbMenu).
const (
	menuCreate   = "create"
	menuMine     = "mine"
	menuHouse    = "house"
	menuSettings = "settings"
	menuHelp     = "help"
	menuSwitch   = "switch"
	menuProblem  = "problem"
	menuMain     = "main" // «В меню»
)

func callbackData(action, param string) string {
	return action + ":" + param
}

func homeCallback(chatID int64) string {
	return callbackData(cbHome, strconv.FormatInt(chatID, 10))
}

func createCallback(parts ...string) string {
	return callbackData(cbCreate, strings.Join(parts, ":"))
}

func voteCallback(value vote.Value, requestID int64) string {
	return callbackData(cbVote, string(value)+":"+strconv.FormatInt(requestID, 10))
}

// parseCallback делит callback_data на код действия и параметр.
func parseCallback(data string) (action, param string) {
	action, param, _ = strings.Cut(data, ":")
	return action, param
}
