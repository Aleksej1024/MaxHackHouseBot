package e2e

// Фейковый MAX Bot API на httptest (e2e без внешней сети): бот ходит в него
// настоящим адаптером maxbot через библиотеку max-bot-api-client-go, поэтому
// проверяются реальные запросы, JSON сообщений и клавиатуры.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

// sentMessage — сообщение бота в текущем состоянии (с учётом правок).
type sentMessage struct {
	Mid         string
	ChatID      int64 // домовой чат; 0 для лички
	UserID      int64 // личка; 0 для чата
	Text        string
	Format      model.TextFormat
	Attachments []model.Attachment
	ReplyTo     string // mid, если сообщение — ответ
	Edits       int
	Deleted     bool
}

// buttons — callback-кнопки сообщения: подпись → payload.
func (m sentMessage) buttons() map[string]string {
	out := map[string]string{}
	for _, a := range m.Attachments {
		if a.Type != model.AttachInlineKeyboard {
			continue
		}
		for _, row := range a.Payload.Buttons {
			for _, b := range row {
				out[b.Text] = b.Payload
			}
		}
	}
	return out
}

// media — токены медиа-вложений (без клавиатуры).
func (m sentMessage) media() []string {
	var out []string
	for _, a := range m.Attachments {
		if a.Type != model.AttachInlineKeyboard {
			out = append(out, string(a.Type)+":"+a.Payload.Token)
		}
	}
	return out
}

type fakeMax struct {
	mu       sync.Mutex
	srv      *httptest.Server
	messages []*sentMessage
	byMid    map[string]*sentMessage
	answers  map[string]string // callback_id → уведомление
	members  map[int64][]model.ChatMember
	titles   map[int64]string
	noDialog map[int64]bool // пользователи, которым бот не может писать
	nextMid  int
}

func newFakeMax() *fakeMax {
	f := &fakeMax{
		byMid:    map[string]*sentMessage{},
		answers:  map[string]string{},
		members:  map[int64][]model.ChatMember{},
		titles:   map[int64]string{},
		noDialog: map[int64]bool{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeMax) Close() { f.srv.Close() }

func (f *fakeMax) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()

	switch {
	case r.URL.Path == "/messages" && r.Method == http.MethodPost:
		var body model.NewMessageBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		userID, _ := strconv.ParseInt(q.Get("user_id"), 10, 64)
		chatID, _ := strconv.ParseInt(q.Get("chat_id"), 10, 64)
		if userID != 0 && f.noDialog[userID] {
			writeJSON(w, http.StatusForbidden, map[string]string{"code": "chat.denied", "message": "dialog not started"})
			return
		}
		f.nextMid++
		m := &sentMessage{
			Mid: fmt.Sprintf("mid.%d", f.nextMid), ChatID: chatID, UserID: userID,
			Text: body.Text, Format: body.Format, Attachments: body.Attachments,
		}
		if body.Link != nil {
			m.ReplyTo = body.Link.Mid
		}
		f.messages = append(f.messages, m)
		f.byMid[m.Mid] = m
		writeJSON(w, http.StatusOK, model.SendMessageResult{Message: model.Message{Body: model.MessageBody{Mid: m.Mid}}})

	case r.URL.Path == "/messages" && r.Method == http.MethodPut:
		var body model.NewMessageBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m, ok := f.byMid[q.Get("message_id")]
		if !ok || m.Deleted {
			writeJSON(w, http.StatusNotFound, map[string]string{"code": "not.found", "message": "message not found"})
			return
		}
		m.Text, m.Attachments = body.Text, body.Attachments
		m.Edits++
		writeJSON(w, http.StatusOK, model.SimpleQueryResult{Success: true})

	case r.URL.Path == "/messages" && r.Method == http.MethodDelete:
		if m, ok := f.byMid[q.Get("message_id")]; ok {
			m.Deleted = true
		}
		writeJSON(w, http.StatusOK, model.SimpleQueryResult{Success: true})

	case r.URL.Path == "/answers" && r.Method == http.MethodPost:
		var body model.CallbackAnswer
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Notification != nil {
			f.answers[q.Get("callback_id")] = *body.Notification
		}
		writeJSON(w, http.StatusOK, model.SimpleQueryResult{Success: true})

	case strings.HasPrefix(r.URL.Path, "/chats/") && strings.HasSuffix(r.URL.Path, "/members"):
		chatID, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/chats/"), "/members"), 10, 64)
		writeJSON(w, http.StatusOK, model.ChatMembersList{Members: f.members[chatID]})

	case strings.HasPrefix(r.URL.Path, "/chats/"):
		chatID, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/chats/"), 10, 64)
		writeJSON(w, http.StatusOK, model.Chat{ChatID: chatID, Title: f.titles[chatID]})

	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"code": "not.found", "message": r.Method + " " + r.URL.Path})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- Доступ к записанному для проверок ---

func (f *fakeMax) snapshot() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentMessage, len(f.messages))
	for i, m := range f.messages {
		out[i] = *m
	}
	return out
}

func (f *fakeMax) message(mid string) sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.byMid[mid]; ok {
		return *m
	}
	return sentMessage{}
}

func (f *fakeMax) answer(callbackID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.answers[callbackID]
}
