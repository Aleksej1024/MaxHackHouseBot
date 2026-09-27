// Package maxbot — адаптер к мессенджеру MAX:
// реализации портов Messenger и MemberLister, роутер апдейтов,
// повторы вызовов (раздел 13) и все тексты для пользователей (раздел 15).
//
// Библиотека max-bot-api-client-go импортируется только здесь и в cmd.
package maxbot

import (
	"maxhouse/internal/domain/comment"
	"maxhouse/internal/domain/problem"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
)

// Тексты сообщений бота в домовой чат
// В логи и callback_data не попадают.
const (
	// ChatGreeting отправляется при добавлении бота в чат,
	// который сразу активирован (AUTO_CHAT_REGISTRATION=true).
	ChatGreeting = "Привет! Я бот домовых чатов. Помогаю жителям объединяться " +
		"вокруг общих тем: жалобы и инициативы соседей.\n\n" +
		"Чтобы начать, напишите мне в личные сообщения — там можно создать заявку, " +
		"посмотреть заявки дома и настроить уведомления."

	// ChatPending отправляется, когда чат ожидает подтверждения организатора
	// (AUTO_CHAT_REGISTRATION=false).
	ChatPending = "Привет! Я бот домовых чатов. Этот чат ожидает подтверждения " +
		"организатором — после него жители смогут создавать заявки и голосовать."
)

// Тексты личного диалога с ботом.
const (
	// DMNotRegistered — пользователь не состоит ни в одном активном домовом чате.
	DMNotRegistered = "Извините, я пока не добавлен в ваш домовой чат. " +
		"Попросите администратора чата добавить меня — после этого здесь " +
		"можно будет создавать заявки и следить за ними."

	// DMChooseChat — пользователь состоит в нескольких домовых чатах.
	DMChooseChat = "Вы состоите в нескольких домовых чатах. Выберите дом, " +
		"с которым хотите работать — сменить его можно в любой момент."

	// DMGreeting показывается один раз за сессию. %s — название домового чата.
	DMGreeting = "Здравствуйте! Вы работаете с домом «%s».\n\n" +
		"Здесь можно:\n" +
		"• создать заявку — жалобу или инициативу для соседей;\n" +
		"• следить за своими заявками и заявками дома;\n" +
		"• добавлять к заявкам комментарии и материалы;\n" +
		"• получать новые заявки дома в личные сообщения;\n\n" +
		"Продолжая общение с ботом, Вы соглашаетесь с условиями использования.\n\n" +
		"Выберите действие:"

	// DMMenu — меню без приветствия. %s — название домового чата.
	DMMenu = "Дом «%s». Выберите действие:"

	// DMHelp — справка по кнопке «Помощь».
	DMHelp = "Как это работает:\n\n" +
		"1. Создайте заявку — она появится в домовом чате с кнопками для соседей.\n" +
		"2. Соседи отмечают, согласны они или нет, и могут прислать вам " +
		"комментарии и материалы в личном диалоге со мной.\n" +
		"3. Когда вопрос решён, закройте заявку и напишите итог — он появится в чате.\n\n" +
		"Автор анонимной заявки соседям не показывается."

	// DMHelpCommands — заголовок списка команд в справке; строки
	// «/команда — описание» добавляются из CommandDescriptions.
	DMHelpCommands = "\n\nВместо кнопок можно писать команды:"

	// ChatUntitled — подпись кнопки выбора чата без названия. %d — id чата.
	ChatUntitled = "Домовой чат %d"
)

// Подписи кнопок меню лички.
const (
	BtnCreateRequest = "Создать заявку"
	BtnMyRequests    = "Мои заявки"
	BtnHouseRequests = "Заявки дома"
	BtnSettings      = "Настройки"
	BtnHelp          = "Помощь"
	BtnSwitchHome    = "Сменить дом"
	BtnReportProblem = "Сообщить о проблеме"
	// BtnToMenu — возврат в меню с экранов без «Отмены» (списки, карточки,
	// настройки, помощь). Прерывает незавершённый диалог, как «Отмена».
	BtnToMenu = "В меню"
)

// CommandDescriptions — описания команд лички (подсказки MAX и справка),
// по имени команды без «/». Тест проверяет, что описание есть у каждой.
var CommandDescriptions = map[string]string{
	"create":   "Создать заявку",
	"problem":  "Сообщить о проблеме",
	"mine":     "Мои заявки",
	"house":    "Заявки дома",
	"settings": "Настройки",
	"switch":   "Сменить дом",
	"help":     "Помощь",
	"menu":     "Меню",
}

// TypeTexts — тексты, зависящие от типа заявки.
// У каждого типа из реестра обязана быть запись в RequestTypeTexts
// (проверяется тестом). В БД голос всегда confirm/refute.
type TypeTexts struct {
	Name            string // название типа для пользователя
	Confirm         string // кнопка согласия (vote.value = confirm)
	Refute          string // кнопка несогласия (vote.value = refute)
	VotedConfirm    string // подтверждение голоса «За»
	VotedRefute     string // подтверждение голоса «Против»
	AudienceAll     string // аудитория рассылки: все проголосовавшие
	AudienceConfirm string // аудитория рассылки: проголосовавшие «За»
	AudienceRefute  string // аудитория рассылки: проголосовавшие «Против»
}

// RequestTypeTexts — тексты по коду типа заявки.
var RequestTypeTexts = map[requesttype.Code]TypeTexts{
	requesttype.Complaint: {
		Name:            "Жалоба",
		Confirm:         "Подтверждаю",
		Refute:          "Опровергаю",
		VotedConfirm:    "Ваш голос учтён: Подтверждаю",
		VotedRefute:     "Ваш голос учтён: Опровергаю",
		AudienceAll:     "Все проголосовавшие",
		AudienceConfirm: "Подтвердившие",
		AudienceRefute:  "Опровергнувшие",
	},
	requesttype.Initiative: {
		Name:            "Инициатива",
		Confirm:         "Подтверждаю",
		Refute:          "Опровергаю",
		VotedConfirm:    "Ваш голос учтён: Подтверждаю",
		VotedRefute:     "Ваш голос учтён: Опровергаю",
		AudienceAll:     "Все проголосовавшие",
		AudienceConfirm: "Подтвердившие",
		AudienceRefute:  "Опровергнувшие",
	},
	requesttype.Poll: {
		Name:            "Опрос",
		Confirm:         "Да",
		Refute:          "Нет",
		VotedConfirm:    "Ваш голос учтён: Да",
		VotedRefute:     "Ваш голос учтён: Нет",
		AudienceAll:     "Все ответившие",
		AudienceConfirm: "Ответившие «Да»",
		AudienceRefute:  "Ответившие «Нет»",
	},
}

// typeTexts возвращает тексты типа; для типа без текстов — нейтральные
// подписи, чтобы не сломать интерфейс (тест не даёт до этого дойти).
func typeTexts(code requesttype.Code) TypeTexts {
	if t, ok := RequestTypeTexts[code]; ok {
		return t
	}
	return RequestTypeTexts[requesttype.Complaint]
}

// Создание заявки в личке.
const (
	CreateChooseType = "Какую заявку хотите создать?"
	// CreateEnterText: %s — тип заявки, %d — максимум символов.
	CreateEnterText = "%s. Опишите суть одним сообщением, до %d символов. " +
		"К сообщению можно сразу приложить фото, видео или файлы."
	// CreateAddMedia: %d — приложено, %d — максимум.
	CreateAddMedia = "Приложено вложений: %d из %d. Пришлите ещё фото, видео или файлы " +
		"либо нажмите «Готово»."
	CreateChooseAnonymity = "Показывать соседям, кто автор заявки?\n\n" +
		"При анонимной публикации автор не виден жителям и администраторам чата."
	// CreateConfirm: %s — тип, %s — автор, %d — вложений, %s — текст.
	CreateConfirm = "Проверьте заявку перед публикацией:\n\n" +
		"Тип: %s\nАвтор: %s\nВложений: %d\n\n%s"
	CreateAuthorShown     = "указан"
	CreateAuthorAnonymous = "анонимно"
	// CreatePublished: %d — номер заявки.
	CreatePublished = "Заявка №%d опубликована в домовом чате."
	// CreatePublishFailed: %d — номер заявки.
	CreatePublishFailed = "Заявка №%d сохранена, но опубликовать её в чате пока не удалось. " +
		"Бот повторит попытку при следующем изменении заявки."
	CreateCancelled = "Создание заявки отменено."
	CreateExpired   = "Черновик устарел, начните заново."
	// CreateLimitReached: %s — тип заявки, %d — суточный лимит на тип.
	CreateLimitReached = "За последние сутки вы создали максимальное число заявок типа «%s» (%d). " +
		"Попробуйте позже."
	CreateRatingTooLow = "Создание заявок сейчас временно ограничено. " +
		"Ограничение снимется со временем."

	ProblemEmptyBody = "Нужен текст заявки."
	// ProblemBodyTooLong: %d — максимум символов.
	ProblemBodyTooLong = "Текст слишком длинный: не больше %d символов."
	// ProblemTooManyMedia: %d — максимум вложений.
	ProblemTooManyMedia = "Можно приложить не больше %d вложений."
	ProblemUnexpected   = "Сейчас нужно другое действие."
	// ProblemTypeLimit: %d — суточный лимит на тип.
	ProblemTypeLimit = "Заявки этого типа на сегодня закончились: не больше %d в сутки. " +
		"Выберите другой тип или попробуйте позже."
)

// Кнопки создания заявки.
const (
	BtnCancel     = "Отмена"
	BtnDone       = "Готово"
	BtnShowAuthor = "Указать меня автором"
	BtnAnonymous  = "Анонимно"
	BtnPublish    = "Опубликовать"
	BtnEditText   = "Изменить текст"
)

// Карточка заявки в домовом чате.
const (
	// CardHeader: %d — номер, %s — тип.
	CardHeader = "Заявка №%d · %s"
	// CardAuthor: %s — ник автора.
	CardAuthor          = "Автор: %s"
	CardAuthorAnonymous = "Автор скрыт"
	// CardVotes: %s — подпись «За», %d, %s — подпись «Против», %d.
	CardVotes = "%s: %d · %s: %d"
	// CardMaterials: %d — число материалов.
	CardMaterials = "Материалов от соседей: %d"
	// Строка статуса в карточке после завершения голосования.
	CardStatusInProgress = "Голосование завершено."
	// CardStatusClosed: %s — итог.
	CardStatusClosed  = "Заявка закрыта автором. Итог: %s"
	CardStatusExpired = "Срок заявки истёк."
	// ResultPosted — итог в чате ответом на карточку. %d — номер, %s — итог.
	ResultPosted = "Итог по заявке №%d:\n\n%s"
)

// StatusNames — статусы заявки для автора в личке.
var StatusNames = map[request.Status]string{
	request.StatusOpen:       "идёт голосование",
	request.StatusInProgress: "голосование завершено",
	request.StatusClosed:     "закрыта",
	request.StatusExpired:    "срок истёк",
	request.StatusDeleted:    "удалена",
}

// Управление своими заявками в личке
const (
	// MyRequestsTitle: %s — дом.
	MyRequestsTitle = "Ваши заявки в доме «%s». Выберите заявку:"
	// MyRequestsEmpty: %s — дом.
	MyRequestsEmpty = "У вас пока нет заявок в доме «%s»."
	// MyRequestItem — кнопка списка. %d — номер, %s — тип, %s — статус.
	MyRequestItem = "№%d · %s · %s"
	// AuthorCard: %d — номер, %s — тип, %s — статус, %s — текст,
	// %s — подпись «За», %d, %s — подпись «Против», %d.
	AuthorCard = "Заявка №%d · %s · %s\n\n%s\n\n%s: %d · %s: %d"
	// AuthorCardResult: %s — итог.
	AuthorCardResult = "Итог: %s"
	// AskResult: %d — номер.
	AskResult = "Напишите итог по заявке №%d одним сообщением — он появится в домовом чате."
	// AskBody: %d — номер.
	AskBody = "Пришлите новый текст заявки №%d. Голоса соседей сохранятся."
	// ConfirmDelete: %d — номер.
	ConfirmDelete = "Удалить заявку №%d? Сообщение в домовом чате тоже будет удалено."
	// RequestClosed: %d — номер.
	RequestClosed = "Заявка №%d закрыта, итог опубликован в домовом чате."
	// RequestEdited: %d — номер.
	RequestEdited = "Текст заявки №%d обновлён."
	// RequestDeleted: %d — номер.
	RequestDeleted = "Заявка №%d удалена."
	// ChatNotSynced — добавляется, если сообщение в чате обновить не удалось.
	ChatNotSynced    = "Обновить сообщение в домовом чате пока не удалось, бот повторит попытку при следующем изменении."
	RequestNotFound  = "Заявка не найдена."
	RequestNotActive = "Заявка уже закрыта или срок её истёк — изменить её нельзя."
	ManageCancelled  = "Действие отменено."
)

// Заявки дома и карточка заявки для жителя.
const (
	// HouseRequestsTitle: %s — дом.
	HouseRequestsTitle = "Открытые заявки дома «%s». Выберите заявку:"
	// HouseRequestsEmpty: %s — дом.
	HouseRequestsEmpty = "В доме «%s» сейчас нет открытых заявок."
	// NeighborCard: %d — номер, %s — тип, %s — статус, %s — текст,
	// %s — строка автора, %s — подпись «За», %d, %s — подпись «Против», %d.
	NeighborCard = "Заявка №%d · %s · %s\n\n%s\n\n%s\n%s: %d · %s: %d"
	// CommentHint: %d — номер. Отправитель должен знать, что автор увидит его ник.
	CommentHint = "Напишите комментарий к заявке №%d одним сообщением. " +
		"Его увидит только автор заявки — вместе с вашим ником."
	// EvidenceHint: %d — номер.
	EvidenceHint = "Пришлите фото, видео или файл к заявке №%d, можно с подписью. " +
		"Их увидит только автор заявки — вместе с вашим ником."
	// MaterialSent: %d — номер.
	MaterialSent       = "Готово, автор заявки №%d получит ваш материал."
	MaterialOwnRequest = "Это ваша заявка — комментарии и доказательства присылают соседи."
	MaterialNotActive  = "Заявка закрыта или срок её истёк — добавить материалы уже нельзя."
	MaterialCancelled  = "Отправка материала отменена."
	ProblemNoMedia     = "Приложите фото, видео или файл."
	// MaterialsEmpty: %d — номер.
	MaterialsEmpty = "К заявке №%d пока нет комментариев и материалов."
	// MaterialItem: %s — вид, %s — ник, %s — время, %s — текст.
	MaterialItem = "%s · %s · %s\n\n%s"
	// MaterialsPage: %d–%d из %d.
	MaterialsPage = "Показаны материалы %d–%d из %d."
	// MaterialNotification — автору о новом материале. %s — вид, %d — номер,
	// %s — ник отправителя, %s — текст.
	MaterialNotification = "%s к вашей заявке №%d от %s:\n\n%s"
	// TimeLayout — формат времени материалов (в часовом поясе DISPLAY_TIMEZONE).
	TimeLayout = "02.01.2006 15:04"
)

// MaterialKindNames — вид материала для автора.
var MaterialKindNames = map[comment.Kind]string{
	comment.KindComment:  "Комментарий",
	comment.KindEvidence: "Доказательство",
}

// Новые заявки дома в личке и открытие заявки из чата.
const (
	// ForwardedRequest — новая заявка в личку. %s — карточка заявки.
	ForwardedRequest  = "Новая заявка в вашем доме.\n\n%s"
	NotifyCardSent    = "Заявка отправлена вам в личные сообщения"
	NotifyStartDialog = "Сначала напишите боту в личные сообщения"
)

// Рассылка по заявке.
const (
	// BroadcastChooseAudience: %d — номер.
	BroadcastChooseAudience = "Кому разослать сообщение по заявке №%d?"
	// BroadcastAskText: %s — аудитория.
	BroadcastAskText = "Аудитория: %s. Напишите текст сообщения — оно придёт от бота " +
		"с номером заявки, ваш ник получатели не увидят."
	// BroadcastConfirm: %d — номер, %s — аудитория, %d — получателей, %s — текст.
	BroadcastConfirm = "Проверьте рассылку по заявке №%d.\nАудитория: %s, получателей: %d.\n\n%s"
	// BroadcastSent: %d — номер, %d — доставлено, %d — получателей.
	BroadcastSent        = "Рассылка по заявке №%d отправлена: доставлено %d из %d."
	BroadcastUndelivered = "Часть жителей не начинали диалог с ботом, им сообщение не доставлено."
	// BroadcastCooldown: %d — номер, %s — время.
	BroadcastCooldown  = "Рассылка по заявке №%d уже была недавно. Следующая возможна после %s."
	BroadcastCancelled = "Рассылка отменена."
	BroadcastExpired   = "Черновик рассылки устарел, начните заново."
	// BroadcastMessage — жителю. %d — номер, %s — тип, %s — текст.
	BroadcastMessage = "Сообщение от автора заявки №%d («%s»):\n\n%s"
)

// Настройки. Рейтинг не показываем, только статус.
// Слово «пересылка» в интерфейсе не используем: его путают с рассылкой.
const (
	// SettingsText: %s — дом, %s — новые заявки в личку,
	// %s — уведомления о комментариях, %s — создание заявок.
	SettingsText = "Настройки для дома «%s».\n\n" +
		"Новые заявки дома в личку: %s.\n" +
		"Уведомления о комментариях к моим заявкам: %s.\n" +
		"Создание заявок: %s."
	SettingOn     = "включено"
	SettingOff    = "выключено"
	CreateAllowed = "доступно"
	CreateLimited = "временно ограничено"
)

// Кнопки этапа 6.
const (
	BtnComment      = "Оставить комментарий"
	BtnEvidence     = "Добавить доказательство"
	BtnMaterials    = "Комментарии и материалы (%d)"
	BtnBroadcast    = "Разослать сообщение"
	BtnMore         = "Показать ещё"
	BtnOpenInDM     = "Открыть в личке"
	BtnOpen         = "Открыть"
	BtnSend         = "Отправить"
	BtnForwardOn    = "Присылать новые заявки"
	BtnForwardOff   = "Не присылать новые заявки"
	BtnNotifyMatOn  = "Уведомлять о комментариях"
	BtnNotifyMatOff = "Не уведомлять о комментариях"
)

// Кнопки управления заявкой.
const (
	BtnEditRequest   = "Изменить текст"
	BtnCloseRequest  = "Закрыть с итогом"
	BtnDeleteRequest = "Удалить"
	BtnConfirmDelete = "Да, удалить"
)

// Ответы на нажатие кнопок голосования в чате.
// Подтверждения голоса зависят от типа и лежат в RequestTypeTexts.
const (
	VoteOwnRequest = "Нельзя голосовать за свою заявку"
	VoteClosed     = "Голосование по этой заявке завершено"
	VoteNotMember  = "Голосовать могут только жители этого дома"
	VoteNotFound   = "Заявка не найдена или удалена"
)

// Короткие уведомления в ответ на нажатие кнопки (callback answer).
const (
	NotifyOK          = "Готово"
	NotifyHomeChosen  = "Дом выбран"
	NotifyUnavailable = "Действие недоступно"
	NotifyError       = "Что-то пошло не так, попробуйте позже"
)

// «Сообщить о проблеме». Название проблемы — по коду;
// у каждого кода из problem.Codes обязано быть название (проверяется тестом).
var ProblemNames = map[problem.Code]string{
	problem.NoInternet: "Нет интернета",
	problem.NoPower:    "Пропало электричество",
	problem.NoWater:    "Пропала вода",
	problem.NoGas:      "Газ отключился",
	problem.Drone:      "Замечен БПЛА",
}

const (
	// ProblemChoose — выбор проблемы в личке.
	ProblemChoose = "Что случилось? Когда о той же проблеме сообщат несколько " +
		"соседей, я предупрежу жителей в домовом чате."
	// ProblemRecorded — сообщение принято. %s — название проблемы.
	ProblemRecorded = "Спасибо, сообщение «%s» учтено."
	// ProblemAlready — житель уже сообщал об этой проблеме в текущем окне.
	ProblemAlready = "Вы уже сообщили о проблеме «%s», сообщение учтено."
	// ProblemAlerted — дописывается к ответу, если сообщение вызвало оповещение.
	ProblemAlerted = "\n\nО проблеме сообщили несколько соседей — жители предупреждены в домовом чате."

	// ProblemAlert — оповещение в домовом чате (markdown). %s — название проблемы.
	ProblemAlert = "Уважаемые жители!\n\nФиксируется массовая проблема «%s»."
	// ProblemAlertAdmins — строка с упоминаниями админов. %s — упоминания через запятую.
	ProblemAlertAdmins = "\n\nАдминистраторы чата: %s"
	// ProblemAdminUnnamed — подпись упоминания админа без ника.
	ProblemAdminUnnamed = "администратор"
)
