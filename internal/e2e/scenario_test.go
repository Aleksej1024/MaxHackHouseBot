package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/adapter/maxbot"
	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/request"
)

var (
	alice = user{ID: 1, Name: "Алиса"}
	bob   = user{ID: 2, Name: "Борис"}
	carol = user{ID: 3, Name: "Карина"}
	dave  = user{ID: 4, Name: "Денис"}
	eve   = user{ID: 5, Name: "Ева"} // не живёт в доме
)

const house = int64(-100)

// myItem — подпись заявки в «Мои заявки».
func myItem(id int64, typeName string, status request.Status) string {
	return fmt.Sprintf(maxbot.MyRequestItem, id, typeName, maxbot.StatusNames[status])
}

// createRequest проводит автора по диалогу создания заявки и возвращает
// сообщение с карточкой в домовом чате.
func (h *harness) createRequest(author user, chatID int64, typeName, text string, anonymous bool, media ...string) sentMessage {
	h.t.Helper()
	before := len(h.chatMessages(chatID))
	h.press(author, maxbot.BtnCreateRequest)
	h.press(author, typeName)
	h.say(author, text, media...)
	h.press(author, maxbot.BtnDone)
	if anonymous {
		h.press(author, maxbot.BtnAnonymous)
	} else {
		h.press(author, maxbot.BtnShowAuthor)
	}
	h.press(author, maxbot.BtnPublish)
	msgs := h.chatMessages(chatID)
	require.Len(h.t, msgs, before+1, "карточка опубликована в чате")
	return msgs[before]
}

func (h *harness) card(mid string) sentMessage { return h.max.message(mid) }

// --- Сценарий 1: регистрация → заявка → голоса → материалы → голосование
// завершено → рассылка → закрытие ---

func TestScenario_FullRequestLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	h.house(house, "Ленина, 5", alice, bob, carol, dave)
	h.noDialog(dave) // Денис с ботом в личке не общался

	// Регистрация: бот добавлен в чат, участники и название чата — из MAX.
	h.botAdded(house, alice)
	chat := h.chatMessages(house)
	require.Len(t, chat, 1)
	assert.Equal(t, maxbot.ChatGreeting, chat[0].Text)

	// Первое сообщение: приветствие один раз, дальше меню.
	h.start(alice)
	assert.Equal(t, fmt.Sprintf(maxbot.DMGreeting, "Ленина, 5"), h.lastDM(alice).Text,
		"название чата получено у MAX (в bot_added его нет)")
	h.start(alice)
	assert.Equal(t, fmt.Sprintf(maxbot.DMMenu, "Ленина, 5"), h.lastDM(alice).Text)

	// Борис включает новые заявки дома в личку. Уведомления о комментариях
	// к своим заявкам включены у всех по умолчанию — Алисе ничего настраивать
	// не нужно.
	h.start(bob)
	h.press(bob, maxbot.BtnSettings)
	assert.Equal(t, fmt.Sprintf(maxbot.SettingsText, "Ленина, 5", maxbot.SettingOff, maxbot.SettingOn, maxbot.CreateAllowed), h.lastDM(bob).Text)
	h.press(bob, maxbot.BtnForwardOn)
	assert.Equal(t, fmt.Sprintf(maxbot.SettingsText, "Ленина, 5", maxbot.SettingOn, maxbot.SettingOn, maxbot.CreateAllowed), h.lastDM(bob).Text)

	// Создание заявки с фото.
	mark := h.mark()
	h.press(alice, maxbot.BtnCreateRequest)
	assert.Equal(t, maxbot.CreateChooseType, h.lastDM(alice).Text)
	h.press(alice, "Жалоба")
	h.say(alice, "Курят в подъезде", "tok-photo")
	assert.Equal(t, fmt.Sprintf(maxbot.CreateAddMedia, 1, 10), h.lastDM(alice).Text)
	h.press(alice, maxbot.BtnDone)
	h.press(alice, maxbot.BtnShowAuthor)
	assert.Equal(t, fmt.Sprintf(maxbot.CreateConfirm, "Жалоба", maxbot.CreateAuthorShown, 1, "Курят в подъезде"), h.lastDM(alice).Text)
	h.press(alice, maxbot.BtnPublish)

	chat = h.chatMessages(house)
	require.Len(t, chat, 2)
	cardMid := chat[1].Mid
	card := h.card(cardMid)
	assert.Equal(t, "Заявка №1 · Жалоба\n\nКурят в подъезде\n\nАвтор: Алиса\nПодтверждаю: 0 · Опровергаю: 0", card.Text)
	assert.Equal(t, []string{"image:tok-photo"}, card.media(), "фото уходит в чат по токену MAX")
	assert.Equal(t, map[string]string{
		"Подтверждаю": "v:confirm:1", "Опровергаю": "v:refute:1", maxbot.BtnOpenInDM: "open:1",
	}, card.buttons())
	assert.Contains(t, h.dmSince(alice, mark), fmt.Sprintf(maxbot.CreatePublished, 1))
	forwarded := h.dmSince(bob, mark)
	require.Len(t, forwarded, 1, "Борис включил новые заявки в личку")
	assert.True(t, strings.HasPrefix(forwarded[0], "Новая заявка в вашем доме."))
	assert.Empty(t, h.dmSince(carol, mark), "у Карины новые заявки в личку выключены (по умолчанию)")

	// Голосование.
	assert.Equal(t, "Ваш голос учтён: Подтверждаю", h.pressInChat(bob, cardMid, "Подтверждаю"))
	assert.Equal(t, "Ваш голос учтён: Подтверждаю", h.pressInChat(carol, cardMid, "Подтверждаю"))
	assert.Equal(t, "Ваш голос учтён: Подтверждаю", h.pressInChat(dave, cardMid, "Подтверждаю"))
	assert.Equal(t, "Ваш голос учтён: Опровергаю", h.pressInChat(dave, cardMid, "Опровергаю"), "голос можно сменить")
	assert.Equal(t, "Ваш голос учтён: Подтверждаю", h.pressInChat(bob, cardMid, "Подтверждаю"), "повтор безопасен")
	assert.Equal(t, maxbot.VoteOwnRequest, h.pressInChat(alice, cardMid, "Подтверждаю"))
	assert.Equal(t, maxbot.VoteNotMember, h.pressInChat(eve, cardMid, "Подтверждаю"))
	assert.Contains(t, h.card(cardMid).Text, "Подтверждаю: 2 · Опровергаю: 1")

	// Материалы: Карина открывает заявку по номеру и оставляет комментарий.
	h.start(carol)
	h.say(carol, "№1")
	neighborCard := h.lastDM(carol)
	assert.True(t, strings.HasPrefix(neighborCard.Text, "Заявка №1 · Жалоба · идёт голосование"))
	assert.Contains(t, neighborCard.Text, "Автор: Алиса")
	mark = h.mark()
	h.press(carol, maxbot.BtnComment)
	assert.Equal(t, fmt.Sprintf(maxbot.CommentHint, 1), h.lastDM(carol).Text)
	h.say(carol, "Видела сама, курят по вечерам")
	assert.Contains(t, h.dmSince(carol, mark), fmt.Sprintf(maxbot.MaterialSent, 1))
	assert.Contains(t, h.dmSince(alice, mark),
		"Комментарий к вашей заявке №1 от Карина:\n\nВидела сама, курят по вечерам",
		"автор видит ник отправителя; уведомления о комментариях включены по умолчанию")
	assert.Contains(t, h.card(cardMid).Text, "Материалов от соседей: 1", "в чат уходит только счётчик")
	assert.NotContains(t, h.card(cardMid).Text, "Видела сама", "текст материала в чат не попадает")

	// «Открыть в личке» из чата.
	assert.Equal(t, maxbot.NotifyCardSent, h.pressInChat(carol, cardMid, maxbot.BtnOpenInDM))
	assert.Equal(t, maxbot.NotifyStartDialog, h.pressInChat(dave, cardMid, maxbot.BtnOpenInDM))

	// Голосование завершается по VOTING_DURATION.
	h.clock.Advance(72*time.Hour + time.Minute)
	h.runJobs()
	card = h.card(cardMid)
	assert.Contains(t, card.Text, maxbot.CardStatusInProgress)
	assert.Equal(t, map[string]string{maxbot.BtnOpenInDM: "open:1"}, card.buttons(), "кнопки голосования убраны")
	assert.Equal(t, 1, h.db.rating(house, alice.ID), "большинство «За» — +1 автору")
	assert.Equal(t, maxbot.VoteClosed, h.callback(dave, card, "v:confirm:1"), "старая кнопка после завершения")

	// Автор смотрит материалы.
	mark = h.mark()
	h.press(alice, maxbot.BtnMyRequests)
	h.press(alice, myItem(1, "Жалоба", request.StatusInProgress))
	h.press(alice, fmt.Sprintf(maxbot.BtnMaterials, 1))
	assert.Equal(t, []string{
		fmt.Sprintf(maxbot.MyRequestsTitle, "Ленина, 5"),
		"Заявка №1 · Жалоба · голосование завершено\n\nКурят в подъезде\n\nПодтверждаю: 2 · Опровергаю: 1",
		"Комментарий · Карина · 21.09.2026 13:00\n\nВидела сама, курят по вечерам",
		fmt.Sprintf(maxbot.MaterialsPage, 1, 1, 1),
	}, h.dmSince(alice, mark), "время — в DISPLAY_TIMEZONE (Москва)")

	// Рассылка всем проголосовавшим: Денису не доставить — он не начинал диалог.
	mark = h.mark()
	h.press(alice, maxbot.BtnBroadcast)
	h.press(alice, "Все проголосовавшие")
	h.say(alice, "Собрание в 19:00 у подъезда")
	assert.Equal(t, fmt.Sprintf(maxbot.BroadcastConfirm, 1, "Все проголосовавшие", 3, "Собрание в 19:00 у подъезда"), h.lastDM(alice).Text)
	h.press(alice, maxbot.BtnSend)
	want := fmt.Sprintf(maxbot.BroadcastMessage, 1, "Жалоба", "Собрание в 19:00 у подъезда")
	assert.Equal(t, []string{want}, h.dmSince(bob, mark))
	assert.Equal(t, []string{want}, h.dmSince(carol, mark))
	assert.NotContains(t, want, "Алиса", "автор в рассылке не раскрывается")
	assert.Contains(t, h.dmSince(alice, mark),
		fmt.Sprintf(maxbot.BroadcastSent, 1, 2, 3)+"\n\n"+maxbot.BroadcastUndelivered)

	// Повторная рассылка — кулдаун.
	h.press(alice, maxbot.BtnMyRequests)
	h.press(alice, myItem(1, "Жалоба", request.StatusInProgress))
	h.press(alice, maxbot.BtnBroadcast)
	assert.Contains(t, h.dmSince(alice, mark), fmt.Sprintf(maxbot.BroadcastCooldown, 1, "25.09.2026 13:01"))

	// Закрытие с итогом.
	mark = h.mark()
	h.press(alice, maxbot.BtnMyRequests)
	h.press(alice, myItem(1, "Жалоба", request.StatusInProgress))
	h.press(alice, maxbot.BtnCloseRequest)
	assert.Equal(t, fmt.Sprintf(maxbot.AskResult, 1), h.lastDM(alice).Text)
	h.say(alice, "Сосед перестал курить")
	assert.Contains(t, h.dmSince(alice, mark), fmt.Sprintf(maxbot.RequestClosed, 1))
	card = h.card(cardMid)
	assert.Contains(t, card.Text, fmt.Sprintf(maxbot.CardStatusClosed, "Сосед перестал курить"))
	chat = h.chatMessages(house)
	result := chat[len(chat)-1]
	assert.Equal(t, fmt.Sprintf(maxbot.ResultPosted, 1, "Сосед перестал курить"), result.Text)
	assert.Equal(t, cardMid, result.ReplyTo, "итог — ответом на карточку")
	assert.Equal(t, 1, h.db.rating(house, alice.ID), "рейтинг не начисляется повторно")

	// Следующий тик планировщика ничего не меняет.
	before := h.mark()
	edits := h.card(cardMid).Edits
	h.runJobs()
	assert.Equal(t, before, h.mark())
	assert.Equal(t, edits, h.card(cardMid).Edits)

	// Денис наконец пишет боту — снова доступен для личных сообщений.
	h.max.mu.Lock()
	delete(h.max.noDialog, dave.ID)
	h.max.mu.Unlock()
	require.True(t, h.db.users[dave.ID].dmUnavailable, "после недоставки рассылки помечен")
	h.start(dave)
	assert.False(t, h.db.users[dave.ID].dmUnavailable)
}

// --- Сценарий 2: опровергнутая заявка → порог рейтинга → сброс ---

func TestScenario_RatingThresholdAndReset(t *testing.T) {
	h := newHarness(t, map[string]string{"RATING_MIN_TO_CREATE": "0"})
	h.house(house, "Ленина, 5", alice, bob, carol)
	h.botAdded(house, alice)
	h.start(alice)
	// Первый тик после деплоя: сброс этой недели уже выполнен.
	h.runJobs()

	card := h.createRequest(alice, house, "Инициатива", "Покрасить лавочки в розовый", false)
	h.pressInChat(bob, card.Mid, "Опровергаю")
	h.pressInChat(carol, card.Mid, "Опровергаю")

	h.clock.Advance(72*time.Hour + time.Minute) // четверг
	h.runJobs()
	assert.Equal(t, -1, h.db.rating(house, alice.ID), "большинство «Против» — −1")

	mark := h.mark()
	h.press(alice, maxbot.BtnCreateRequest)
	assert.Contains(t, h.dmSince(alice, mark), maxbot.CreateRatingTooLow, "ниже RATING_MIN_TO_CREATE")
	h.press(alice, maxbot.BtnSettings)
	assert.Contains(t, h.lastDM(alice).Text, maxbot.CreateLimited)
	assert.NotContains(t, h.lastDM(alice).Text, "-1", "число рейтинга не показывается")

	h.clock.Advance(4 * 24 * time.Hour) // следующий понедельник
	h.runJobs()
	assert.Equal(t, 0, h.db.rating(house, alice.ID), "еженедельный сброс отрицательного рейтинга")

	h.press(alice, maxbot.BtnCreateRequest)
	assert.Equal(t, maxbot.CreateChooseType, h.lastDM(alice).Text)
}

// --- Сценарий 3: подтверждение чата, анонимность, правка, удаление,
// суточный лимит, истечение срока, выход и переименование ---

func TestScenario_PendingChatEditDeleteExpire(t *testing.T) {
	h := newHarness(t, map[string]string{
		"AUTO_CHAT_REGISTRATION": "false",
		"DAILY_REQUEST_LIMIT":    "1",
	})
	const home = int64(-200)
	h.house(home, "Мира, 1", alice, bob)

	// Чат ждёт подтверждения организатора.
	h.botAdded(home, alice)
	assert.Equal(t, maxbot.ChatPending, h.chatMessages(home)[0].Text)
	h.start(alice)
	assert.Equal(t, maxbot.DMNotRegistered, h.lastDM(alice).Text, "pending выглядит как отсутствие чата")
	// Организатор активирует чат в БД: UPDATE house_chats SET status = 'active'.
	require.NoError(t, memChats{h.db}.SetStatus(context.Background(), home, housechat.StatusActive))
	h.start(alice)
	assert.Equal(t, fmt.Sprintf(maxbot.DMGreeting, "Мира, 1"), h.lastDM(alice).Text)

	// Анонимная заявка: автор скрыт в чате и в карточке соседа.
	card := h.createRequest(alice, home, "Жалоба", "Мусор у второго подъезда", true)
	assert.Contains(t, card.Text, maxbot.CardAuthorAnonymous)
	assert.NotContains(t, card.Text, "Алиса")
	h.start(bob)
	h.say(bob, "1")
	assert.Contains(t, h.lastDM(bob).Text, maxbot.CardAuthorAnonymous)

	// Уведомления о комментариях: по умолчанию приходят, после отключения — нет.
	mark := h.mark()
	h.press(bob, maxbot.BtnComment)
	h.say(bob, "Подтверждаю, пакеты лежат третий день")
	require.Len(t, h.dmSince(alice, mark), 1, "автору анонимной заявки уведомление тоже приходит")
	h.press(alice, maxbot.BtnSettings)
	h.press(alice, maxbot.BtnNotifyMatOff)
	assert.Contains(t, h.lastDM(alice).Text, "Уведомления о комментариях к моим заявкам: выключено.")
	mark = h.mark()
	h.say(bob, "1")
	h.press(bob, maxbot.BtnComment)
	h.say(bob, "И ещё коробки")
	assert.Empty(t, h.dmSince(alice, mark), "уведомления выключены")

	// Правка текста: голоса сохраняются.
	h.pressInChat(bob, card.Mid, "Подтверждаю")
	h.press(alice, maxbot.BtnMyRequests)
	h.press(alice, myItem(1, "Жалоба", request.StatusOpen))
	h.press(alice, maxbot.BtnEditRequest)
	h.say(alice, "Мусор у второго подъезда уже неделю")
	assert.Contains(t, h.card(card.Mid).Text, "Мусор у второго подъезда уже неделю")
	assert.Contains(t, h.card(card.Mid).Text, "Подтверждаю: 1")

	// Удаление с подтверждением: сообщение в чате удаляется.
	h.press(alice, maxbot.BtnMyRequests)
	h.press(alice, myItem(1, "Жалоба", request.StatusOpen))
	h.press(alice, maxbot.BtnDeleteRequest)
	assert.False(t, h.card(card.Mid).Deleted, "без подтверждения не удаляем")
	h.press(alice, maxbot.BtnConfirmDelete)
	assert.True(t, h.card(card.Mid).Deleted)
	h.say(bob, "1")
	assert.Equal(t, fmt.Sprintf(maxbot.DMMenu, "Мира, 1"), h.lastDM(bob).Text)
	assert.Contains(t, h.dmLog(bob, 2), maxbot.RequestNotFound)

	// Суточный лимит (1) действует на каждый тип отдельно: жалоба уже была
	// (удалённая тоже считается), а инициативу создать можно.
	mark = h.mark()
	h.press(alice, maxbot.BtnCreateRequest)
	h.press(alice, "Жалоба")
	assert.Equal(t, fmt.Sprintf(maxbot.ProblemTypeLimit, 1)+"\n\n"+maxbot.CreateChooseType, h.lastDM(alice).Text,
		"по исчерпанному типу — снова выбор типа")
	h.press(alice, maxbot.BtnCancel)
	initiative := h.createRequest(alice, home, "Инициатива", "Ставим шлагбаум?", false)
	assert.Equal(t, "Подтверждаю", firstKey(initiative.buttons(), "v:confirm:2"), "подписи кнопок инициативы")
	h.press(alice, maxbot.BtnCreateRequest)
	h.press(alice, "Инициатива")
	assert.Contains(t, h.lastDM(alice).Text, fmt.Sprintf(maxbot.ProblemTypeLimit, 1), "теперь исчерпан и лимит инициатив")
	h.press(alice, maxbot.BtnCancel)
	assert.NotContains(t, h.dmSince(alice, mark), fmt.Sprintf(maxbot.CreateLimitReached, "Жалоба", 1))

	// Истечение общего срока.
	h.clock.Advance(14*24*time.Hour + time.Minute)
	h.runJobs()
	assert.Contains(t, h.card(initiative.Mid).Text, maxbot.CardStatusExpired)
	assert.Equal(t, map[string]string{maxbot.BtnOpenInDM: "open:2"}, h.card(initiative.Mid).buttons())

	// Переименование чата подхватывается.
	h.send(map[string]any{"update_type": "chat_title_changed", "chat_id": home, "title": "Мира, 1 корпус 2", "user": alice.json()})
	h.press(alice, maxbot.BtnSettings)
	assert.True(t, strings.HasPrefix(h.lastDM(alice).Text, "Настройки для дома «Мира, 1 корпус 2»."))

	// Борис вышел из чата — бот для него снова «не добавлен».
	h.userRemoved(home, bob)
	h.start(bob)
	assert.Equal(t, maxbot.DMNotRegistered, h.lastDM(bob).Text)
	// И вернулся.
	h.userAdded(home, bob)
	h.start(bob)
	assert.Equal(t, fmt.Sprintf(maxbot.DMGreeting, "Мира, 1 корпус 2"), h.lastDM(bob).Text,
		"сессия истекла за 14 дней (SESSION_TTL) — приветствие снова")
}

// firstKey — подпись кнопки с заданным payload.
func firstKey(buttons map[string]string, payload string) string {
	for label, p := range buttons {
		if p == payload {
			return label
		}
	}
	return ""
}

// --- Сценарий 4: «В меню» прерывает незавершённый диалог ---

func TestScenario_ToMenuInterruptsDraft(t *testing.T) {
	h := newHarness(t, nil)
	h.house(house, "Ленина, 5", alice, bob)
	h.botAdded(house, alice)
	h.start(alice)

	// Алиса начала заявку, ушла в «Настройки» и нажала там «В меню».
	h.press(alice, maxbot.BtnCreateRequest)
	h.press(alice, "Жалоба")
	assert.Equal(t, fmt.Sprintf(maxbot.CreateEnterText, "Жалоба", 3000), h.lastDM(alice).Text)
	h.press(alice, maxbot.BtnSettings)
	h.press(alice, maxbot.BtnToMenu)
	assert.Equal(t, fmt.Sprintf(maxbot.DMMenu, "Ленина, 5"), h.lastDM(alice).Text)

	// Текст больше не уходит в брошенный черновик — просто меню.
	h.say(alice, "Это не должно стать заявкой")
	assert.Equal(t, fmt.Sprintf(maxbot.DMMenu, "Ленина, 5"), h.lastDM(alice).Text)
	assert.Len(t, h.chatMessages(house), 1, "в чате только приветствие, заявки нет")

	// Новая заявка создаётся с чистого листа.
	card := h.createRequest(alice, house, "Инициатива", "Посадить клёны", false)
	assert.True(t, strings.HasPrefix(card.Text, "Заявка №1 · Инициатива\n\nПосадить клёны"))

	// «В меню» есть и на карточке, и в списках.
	h.press(alice, maxbot.BtnMyRequests)
	h.press(alice, myItem(1, "Инициатива", request.StatusOpen))
	h.press(alice, maxbot.BtnToMenu)
	assert.Equal(t, fmt.Sprintf(maxbot.DMMenu, "Ленина, 5"), h.lastDM(alice).Text)
}

// --- Сообщить о проблеме: оповещение в чат, когда за окно сообщили
// PROBLEM_ALERT_THRESHOLD разных жителей ---

func TestScenario_ProblemAlert(t *testing.T) {
	h := newHarness(t, nil) // порог 3, окно 30m по умолчанию
	h.house(house, "Ленина, 5", alice, bob, carol, dave)
	h.botAdded(house, alice) // Алиса — владелец чата
	for _, u := range []user{alice, bob, carol, dave, eve} {
		h.start(u)
	}
	water := maxbot.ProblemNames["no_water"]
	report := func(u user, name string) string {
		h.t.Helper()
		h.press(u, maxbot.BtnReportProblem)
		assert.Equal(t, maxbot.ProblemChoose, h.lastDM(u).Text)
		h.press(u, name)
		return h.lastDM(u).Text
	}
	alerts := func() []sentMessage {
		var out []sentMessage
		for _, m := range h.chatMessages(house) {
			if strings.HasPrefix(m.Text, "Уважаемые жители!") {
				out = append(out, m)
			}
		}
		return out
	}

	// Житель не из дома кнопки не видит: бот его не знает.
	h.say(eve, "привет")
	assert.Equal(t, maxbot.DMNotRegistered, h.lastDM(eve).Text)

	assert.Equal(t, fmt.Sprintf(maxbot.ProblemRecorded, water), report(bob, water))
	assert.Equal(t, fmt.Sprintf(maxbot.ProblemAlready, water), report(bob, water), "повтор не считается")
	h.clock.Advance(10 * time.Minute)
	report(carol, maxbot.ProblemNames["no_power"]) // другая проблема — отдельный счёт
	report(carol, water)
	assert.Empty(t, alerts(), "двое из трёх")

	h.clock.Advance(15 * time.Minute)
	assert.Equal(t, fmt.Sprintf(maxbot.ProblemRecorded, water)+maxbot.ProblemAlerted, report(dave, water))
	got := alerts()
	require.Len(t, got, 1, "третий житель в окне — оповещение")
	assert.Equal(t, "Уважаемые жители!\n\nФиксируется массовая проблема «Пропала вода»."+
		"\n\nАдминистраторы чата: [Алиса](max://user/1)", got[0].Text)
	assert.Equal(t, "markdown", string(got[0].Format), "упоминание — markdown-ссылка")

	// Четвёртый в том же окне — без второго оповещения.
	assert.Equal(t, fmt.Sprintf(maxbot.ProblemRecorded, water), report(alice, water))
	assert.Len(t, alerts(), 1)

	// Окно (30 минут с первого сообщения) закрылось — счёт заново.
	h.clock.Advance(10 * time.Minute)
	report(bob, water)
	report(carol, water)
	assert.Len(t, alerts(), 1)
	report(dave, water)
	assert.Len(t, alerts(), 2, "новое окно — новое оповещение")

	// «В меню» после ответа.
	h.press(dave, maxbot.BtnToMenu)
	assert.Equal(t, fmt.Sprintf(maxbot.DMMenu, "Ленина, 5"), h.lastDM(dave).Text)
}

// --- Текстовые команды лички дублируют меню ---

func TestScenario_Commands(t *testing.T) {
	h := newHarness(t, nil)
	h.house(house, "Ленина, 5", alice, bob)
	h.botAdded(house, alice)
	h.start(alice)

	h.say(alice, "/create")
	h.press(alice, "Жалоба")
	assert.Equal(t, fmt.Sprintf(maxbot.CreateEnterText, "Жалоба", 3000), h.lastDM(alice).Text)

	// Команда посреди черновика прерывает его и не становится текстом заявки.
	h.say(alice, "/problem")
	assert.Equal(t, maxbot.ProblemChoose, h.lastDM(alice).Text)
	h.say(alice, "Это не должно стать заявкой")
	assert.Equal(t, fmt.Sprintf(maxbot.DMMenu, "Ленина, 5"), h.lastDM(alice).Text)
	assert.Len(t, h.chatMessages(house), 1, "в чате только приветствие")

	h.say(alice, "/help")
	assert.Contains(t, h.lastDM(alice).Text, "/create — Создать заявку")
	h.say(alice, "/settings")
	assert.True(t, strings.HasPrefix(h.lastDM(alice).Text, "Настройки"), h.lastDM(alice).Text)
	h.say(alice, "/menu")
	assert.Equal(t, fmt.Sprintf(maxbot.DMMenu, "Ленина, 5"), h.lastDM(alice).Text)

	// Команды работают с кнопками вперемешку: создали заявку командой до конца.
	h.say(alice, "/create")
	h.press(alice, "Инициатива")
	h.say(alice, "Посадить клёны")
	h.press(alice, maxbot.BtnDone)
	h.press(alice, maxbot.BtnShowAuthor)
	h.press(alice, maxbot.BtnPublish)
	assert.Len(t, h.chatMessages(house), 2, "заявка опубликована")
	h.say(alice, "/mine")
	assert.NotEmpty(t, h.lastDM(alice).buttons()[myItem(1, "Инициатива", request.StatusOpen)])
}

// --- Выход из выбранного дома: сессия переходит на оставшийся дом,
// после выхода из последнего — полностью сбрасывается ---

func TestScenario_LeaveSelectedHouse(t *testing.T) {
	const houseB = int64(-200)
	h := newHarness(t, nil)
	h.house(house, "Ленина, 5", alice, bob)
	h.house(houseB, "Мира, 1", carol, bob)
	h.botAdded(house, alice)
	h.botAdded(houseB, carol)

	// Борис выбрал «Ленина, 5» и начал там заявку.
	h.start(bob)
	assert.Equal(t, maxbot.DMChooseChat, h.lastDM(bob).Text)
	h.press(bob, "Ленина, 5")
	h.press(bob, maxbot.BtnCreateRequest)
	h.press(bob, "Жалоба")

	// Вышел из «Ленина, 5»: черновик прерван, активный дом — «Мира, 1».
	h.userRemoved(house, bob)
	h.say(bob, "Текст после выхода")
	assert.Equal(t, fmt.Sprintf(maxbot.DMMenu, "Мира, 1"), h.lastDM(bob).Text,
		"сразу меню оставшегося дома, без выбора и без черновика")
	assert.Len(t, h.chatMessages(house), 1, "в «Ленина, 5» заявка не появилась")

	// Всё дальше — в «Мира, 1».
	card := h.createRequest(bob, houseB, "Жалоба", "Шум в подвале", false)
	assert.True(t, strings.HasPrefix(card.Text, "Заявка №1 · Жалоба"))

	// Вышел и из «Мира, 1», посреди нового черновика: сброс полностью.
	h.press(bob, maxbot.BtnCreateRequest)
	h.press(bob, "Инициатива")
	h.userRemoved(houseB, bob)
	mark := h.mark()
	h.say(bob, "Ещё текст")
	assert.Equal(t, []string{maxbot.DMNotRegistered}, h.dmSince(bob, mark))
	mark = h.mark()
	h.say(bob, "/house")
	assert.Equal(t, []string{maxbot.DMNotRegistered}, h.dmSince(bob, mark))

	// Вернулся — снова с приветствием: сессия была сброшена целиком.
	h.userAdded(houseB, bob)
	h.start(bob)
	assert.Equal(t, fmt.Sprintf(maxbot.DMGreeting, "Мира, 1"), h.lastDM(bob).Text)
}
