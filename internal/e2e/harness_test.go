// Package e2e — сквозные сценарии без внешней сети:
// in-memory хранилища, настоящие сценарии и транспорт (app.Build) и
// фейковый MAX API на httptest. Апдейты приходят JSON-ом MAX через
// обработчик библиотеки, пользователи нажимают кнопки из сообщений,
// которые бот действительно отправил.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	maxbotapi "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/adapter/maxbot"
	"maxhouse/internal/app"
	"maxhouse/internal/config"
)

const (
	secret = "e2e-secret"
	botID  = int64(999)
)

var t0 = time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC) // понедельник

type harness struct {
	t       *testing.T
	clock   *fakeClock
	db      *memDB
	max     *fakeMax
	bot     *app.App
	handler http.Handler
	seq     int
}

// newHarness собирает бота. env — переопределения конфига поверх значений
// по умолчанию (как в .env).
func newHarness(t *testing.T, env map[string]string) *harness {
	t.Helper()
	isolateEnv(t)
	t.Setenv("MAX_BOT_TOKEN", "e2e-token")
	t.Setenv("POSTGRES_DSN", "postgres://unused")
	t.Setenv("REDIS_ADDR", "unused:6379")
	t.Setenv("UPDATES_MODE", "longpoll")
	t.Setenv("MAX_RETRY_ATTEMPTS", "1")
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	require.NoError(t, err)

	fm := newFakeMax()
	t.Cleanup(fm.Close)
	api, err := maxbotapi.NewApi("e2e-token", maxbotapi.WithBaseURL(fm.srv.URL))
	require.NoError(t, err)
	retry := maxbot.RetryOptions{Attempts: cfg.MaxBot.RetryAttempts, CallTimeout: cfg.MaxBot.CallTimeout}

	clock := &fakeClock{now: t0}
	db := newMemDB(clock)
	adapters := db.adapters()
	adapters.Members = maxbot.NewMemberLister(api, retry)
	adapters.Messenger = maxbot.NewMessenger(api, retry)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bot, err := app.Build(cfg, adapters, log)
	require.NoError(t, err)

	return &harness{
		t: t, clock: clock, db: db, max: fm, bot: bot,
		// Тот же обработчик, что на /webhook: разбор JSON библиотекой MAX.
		// Роутер вызывается синхронно — сценарий детерминирован.
		handler: api.GetHandler(bot.Router.Process, secret),
	}
}

// isolateEnv сбрасывает переменные бота, экспортированные в окружение
// разработчика (например, из .env): сценарии работают на значениях по
// умолчанию. Имена берутся из исходника конфига.
func isolateEnv(t *testing.T) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "config", "config.go"))
	require.NoError(t, err)
	for _, m := range regexp.MustCompile(`(?:env\w*|required)\("([A-Z][A-Z0-9_]+)"`).FindAllSubmatch(src, -1) {
		t.Setenv(string(m[1]), "")
	}
}

// --- Пользователи и чаты ---

type user struct {
	ID   int64
	Name string
}

func (u user) json() map[string]any {
	return map[string]any{"user_id": u.ID, "name": u.Name, "first_name": u.Name, "is_bot": false}
}

func dialogID(u user) int64 { return 1_000_000 + u.ID }

// house задаёт в фейковом MAX чат с участниками (бот тоже участник).
func (h *harness) house(chatID int64, title string, residents ...user) {
	h.max.mu.Lock()
	defer h.max.mu.Unlock()
	h.max.titles[chatID] = title
	members := []model.ChatMember{{UserID: botID, Name: "Бот", IsBot: true}}
	for i, u := range residents {
		members = append(members, model.ChatMember{UserID: u.ID, Name: u.Name, IsOwner: i == 0})
	}
	h.max.members[chatID] = members
}

// noDialog — пользователь не начинал диалог с ботом: личка вернёт 403.
func (h *harness) noDialog(u user) {
	h.max.mu.Lock()
	defer h.max.mu.Unlock()
	h.max.noDialog[u.ID] = true
}

// --- Отправка апдейтов ---

func (h *harness) send(upd map[string]any) {
	h.t.Helper()
	h.seq++
	upd["timestamp"] = h.clock.Now().UnixMilli() + int64(h.seq)
	body, err := json.Marshal(upd)
	require.NoError(h.t, err)
	req := httptest.NewRequestWithContext(h.t.Context(), http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set(maxbotapi.SecretHeader, secret)
	rr := httptest.NewRecorder()
	h.handler.ServeHTTP(rr, req)
	require.Equal(h.t, http.StatusOK, rr.Code, rr.Body.String())
}

func (h *harness) botAdded(chatID int64, by user) {
	h.send(map[string]any{"update_type": "bot_added", "chat_id": chatID, "user": by.json(), "is_channel": false})
}

func (h *harness) userAdded(chatID int64, u user) {
	h.send(map[string]any{"update_type": "user_added", "chat_id": chatID, "user": u.json(), "user_id": u.ID})
}

func (h *harness) userRemoved(chatID int64, u user) {
	h.send(map[string]any{"update_type": "user_removed", "chat_id": chatID, "user": u.json(), "user_id": u.ID})
}

func (h *harness) start(u user) {
	h.send(map[string]any{"update_type": "bot_started", "chat_id": dialogID(u), "user": u.json(), "user_id": u.ID})
}

// say — сообщение пользователя в личку боту; media — токены фото.
func (h *harness) say(u user, text string, media ...string) {
	var atts []map[string]any
	for _, tok := range media {
		atts = append(atts, map[string]any{"type": "image", "payload": map[string]any{"token": tok}})
	}
	h.seq++
	h.send(map[string]any{
		"update_type": "message_created",
		"message": map[string]any{
			"recipient": map[string]any{"chat_id": dialogID(u), "chat_type": "dialog", "user_id": botID},
			"sender":    u.json(),
			"body":      map[string]any{"mid": fmt.Sprintf("user.mid.%d", h.seq), "text": text, "attachments": atts},
		},
	})
}

// callback — нажатие кнопки с payload на сообщении msg; возвращает ответ
// бота (уведомление).
func (h *harness) callback(u user, msg sentMessage, payload string) string {
	h.t.Helper()
	h.seq++
	cbID := fmt.Sprintf("cb.%d", h.seq)
	recipient := map[string]any{"chat_id": dialogID(u), "chat_type": "dialog", "user_id": u.ID}
	if msg.ChatID != 0 {
		recipient = map[string]any{"chat_id": msg.ChatID, "chat_type": "chat"}
	}
	h.send(map[string]any{
		"update_type": "message_callback",
		"callback":    map[string]any{"callback_id": cbID, "payload": payload, "user": u.json()},
		"message":     map[string]any{"recipient": recipient, "body": map[string]any{"mid": msg.Mid, "text": msg.Text}},
	})
	return h.max.answer(cbID)
}

// press нажимает кнопку с подписью label в последнем сообщении лички,
// где такая кнопка есть.
func (h *harness) press(u user, label string) string {
	h.t.Helper()
	msgs := h.max.snapshot()
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.UserID != u.ID || m.Deleted {
			continue
		}
		if payload, ok := m.buttons()[label]; ok {
			return h.callback(u, m, payload)
		}
	}
	h.t.Fatalf("у пользователя %d нет кнопки %q; последние сообщения:\n%s", u.ID, label, h.dmLog(u, 3))
	return ""
}

// pressInChat нажимает кнопку под сообщением в домовом чате.
func (h *harness) pressInChat(u user, mid, label string) string {
	h.t.Helper()
	m := h.max.message(mid)
	payload, ok := m.buttons()[label]
	require.True(h.t, ok, "под сообщением %s нет кнопки %q (есть %v)", mid, label, m.buttons())
	return h.callback(u, m, payload)
}

// --- Проверки ---

// mark — позиция в журнале сообщений, чтобы смотреть только новые.
func (h *harness) mark() int { return len(h.max.snapshot()) }

// dmSince — тексты сообщений пользователю после позиции mark.
func (h *harness) dmSince(u user, from int) []string {
	var out []string
	for _, m := range h.max.snapshot()[from:] {
		if m.UserID == u.ID {
			out = append(out, m.Text)
		}
	}
	return out
}

// lastDM — последнее сообщение пользователю.
func (h *harness) lastDM(u user) sentMessage {
	msgs := h.max.snapshot()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].UserID == u.ID {
			return msgs[i]
		}
	}
	return sentMessage{}
}

// chatMessages — сообщения бота в домовом чате (текущее состояние).
func (h *harness) chatMessages(chatID int64) []sentMessage {
	var out []sentMessage
	for _, m := range h.max.snapshot() {
		if m.ChatID == chatID {
			out = append(out, m)
		}
	}
	return out
}

func (h *harness) dmLog(u user, n int) string {
	var lines []string
	for _, m := range h.max.snapshot() {
		if m.UserID == u.ID {
			lines = append(lines, fmt.Sprintf("  [%s] %q кнопки=%v", m.Mid, m.Text, m.buttons()))
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// runJobs — один тик планировщика.
func (h *harness) runJobs() {
	h.t.Helper()
	ctx := context.Background()
	require.NoError(h.t, h.bot.Lifecycle.Run(ctx))
	require.NoError(h.t, h.bot.RatingReset.Run(ctx))
}
