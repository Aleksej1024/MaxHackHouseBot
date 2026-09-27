// Package onboarding — сценарий первого сообщения пользователя в личке
// проверка, что пользователь состоит в активном
// домовом чате, выбор активного чата и однократное приветствие за сессию.
//
// Сессия хранится в FSMStore (Redis) по ключам session:{user_id}:chat_id
// и session:{user_id}:greeted. Потеря Redis не ломает сценарий: активный
// чат определяется заново, в худшем случае пользователь выберет дом ещё раз.
//
// Use case не содержит текстов: транспорт выбирает их по Result.
package onboarding

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/usecase"
)

// Outcome — итог обращения пользователя в личке.
type Outcome int

const (
	// OutcomeNotRegistered — пользователь не состоит ни в одном активном чате.
	OutcomeNotRegistered Outcome = iota + 1
	// OutcomeChooseChat — чатов несколько, нужно выбрать активный.
	OutcomeChooseChat
	// OutcomeReady — активный чат определён, можно показывать меню.
	OutcomeReady
)

// Result — что показать пользователю.
type Result struct {
	Outcome Outcome
	// Chats — доступные чаты (для OutcomeChooseChat и OutcomeReady).
	Chats []housechat.Chat
	// Chat — активный чат (только для OutcomeReady).
	Chat housechat.Chat
	// Greet — приветствие ещё не показывалось в этой сессии.
	Greet bool
}

// CanSwitch — у пользователя больше одного чата, в меню нужна кнопка «Сменить дом».
func (r Result) CanSwitch() bool { return len(r.Chats) > 1 }

// Service реализует сценарий первого сообщения.
type Service struct {
	dir        usecase.MemberDirectory
	store      usecase.FSMStore
	sessionTTL time.Duration
	log        *slog.Logger
}

func New(dir usecase.MemberDirectory, store usecase.FSMStore, sessionTTL time.Duration, log *slog.Logger) *Service {
	return &Service{dir: dir, store: store, sessionTTL: sessionTTL, log: log}
}

// Start обрабатывает любое обращение пользователя в личке (старт диалога,
// сообщение вне сценариев): обновляет ник и определяет активный чат.
func (s *Service) Start(ctx context.Context, u membership.User) (Result, error) {
	if err := s.dir.UpdateNickname(ctx, u.ID, u.Nickname); err != nil {
		// Ник вторичен, сценарий продолжается.
		s.log.Warn("обновление ника", "user_id", u.ID, "error", err)
	}

	chats, err := s.dir.ListActiveChats(ctx, u.ID)
	if err != nil {
		return Result{}, fmt.Errorf("чаты пользователя: %w", err)
	}
	if len(chats) == 0 {
		return Result{Outcome: OutcomeNotRegistered}, nil
	}

	if id, ok := s.sessionChat(ctx, u.ID); ok {
		if chat, found := findChat(chats, id); found {
			return s.ready(ctx, u.ID, chat, chats), nil
		}
	}
	if len(chats) == 1 {
		return s.ready(ctx, u.ID, chats[0], chats), nil
	}
	return Result{Outcome: OutcomeChooseChat, Chats: chats}, nil
}

// SelectChat делает чат активным по нажатию кнопки. Данным кнопки не доверяем:
// если пользователь больше не участник этого чата, возвращается выбор заново.
func (s *Service) SelectChat(ctx context.Context, userID, chatID int64) (Result, error) {
	chats, err := s.dir.ListActiveChats(ctx, userID)
	if err != nil {
		return Result{}, fmt.Errorf("чаты пользователя: %w", err)
	}
	if len(chats) == 0 {
		return Result{Outcome: OutcomeNotRegistered}, nil
	}
	chat, ok := findChat(chats, chatID)
	if !ok {
		return Result{Outcome: OutcomeChooseChat, Chats: chats}, nil
	}
	return s.ready(ctx, userID, chat, chats), nil
}

// SwitchChat — кнопка «Сменить дом»: предлагает выбрать чат заново.
// Если чат один, выбирать нечего, возвращается обычный Ready.
func (s *Service) SwitchChat(ctx context.Context, userID int64) (Result, error) {
	chats, err := s.dir.ListActiveChats(ctx, userID)
	if err != nil {
		return Result{}, fmt.Errorf("чаты пользователя: %w", err)
	}
	switch len(chats) {
	case 0:
		return Result{Outcome: OutcomeNotRegistered}, nil
	case 1:
		return s.ready(ctx, userID, chats[0], chats), nil
	}
	return Result{Outcome: OutcomeChooseChat, Chats: chats}, nil
}

// ActiveChat возвращает активный чат пользователя для других сценариев
// (создание заявки, настройки). ok = false, если чат не выбран или больше
// не доступен — тогда транспорт должен вызвать Start.
func (s *Service) ActiveChat(ctx context.Context, userID int64) (housechat.Chat, bool, error) {
	chats, err := s.dir.ListActiveChats(ctx, userID)
	if err != nil {
		return housechat.Chat{}, false, fmt.Errorf("чаты пользователя: %w", err)
	}
	if id, ok := s.sessionChat(ctx, userID); ok {
		if chat, found := findChat(chats, id); found {
			return chat, true, nil
		}
	}
	if len(chats) == 1 {
		s.setSession(ctx, userID, sessionChatKey(userID), strconv.FormatInt(chats[0].ID, 10))
		return chats[0], true, nil
	}
	return housechat.Chat{}, false, nil
}

// LeftChat обновляет сессию пользователя, вышедшего из чата chatID
// (статус участия уже left). Если в сессии выбран другой дом, ничего не
// меняется (switched = false). Если выбран этот дом:
//   - других домов нет — сессия удаляется целиком (дом и приветствие);
//   - есть другие — активным становится первый из оставшихся.
//
// switched = true — выбранный дом сменился или сброшен: незавершённый диалог
// по ушедшему дому вызывающий должен прервать.
func (s *Service) LeftChat(ctx context.Context, userID, chatID int64) (switched bool, err error) {
	v, found, err := s.store.Get(ctx, sessionChatKey(userID))
	if err != nil {
		return false, fmt.Errorf("чтение сессии: %w", err)
	}
	if !found || v != strconv.FormatInt(chatID, 10) {
		return false, nil
	}
	chats, err := s.dir.ListActiveChats(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("чаты пользователя: %w", err)
	}
	if len(chats) == 0 {
		for _, key := range []string{sessionChatKey(userID), sessionGreetedKey(userID)} {
			if err := s.store.Delete(ctx, key); err != nil {
				return false, fmt.Errorf("сброс сессии: %w", err)
			}
		}
		return true, nil
	}
	if err := s.store.Set(ctx, sessionChatKey(userID), strconv.FormatInt(chats[0].ID, 10), s.sessionTTL); err != nil {
		return false, fmt.Errorf("смена дома в сессии: %w", err)
	}
	return true, nil
}

// ready сохраняет активный чат в сессии (продлевая TTL) и решает,
// показывать ли приветствие.
func (s *Service) ready(ctx context.Context, userID int64, chat housechat.Chat, chats []housechat.Chat) Result {
	s.setSession(ctx, userID, sessionChatKey(userID), strconv.FormatInt(chat.ID, 10))

	greet := true
	if _, found, err := s.store.Get(ctx, sessionGreetedKey(userID)); err != nil {
		s.log.Warn("чтение сессии", "user_id", userID, "error", err)
	} else if found {
		greet = false
	}
	s.setSession(ctx, userID, sessionGreetedKey(userID), "1")

	return Result{Outcome: OutcomeReady, Chats: chats, Chat: chat, Greet: greet}
}

// sessionChat читает активный чат из сессии. Ошибка Redis не прерывает
// сценарий: считаем, что сессии нет.
func (s *Service) sessionChat(ctx context.Context, userID int64) (int64, bool) {
	v, found, err := s.store.Get(ctx, sessionChatKey(userID))
	if err != nil {
		s.log.Warn("чтение сессии", "user_id", userID, "error", err)
		return 0, false
	}
	if !found {
		return 0, false
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func (s *Service) setSession(ctx context.Context, userID int64, key, value string) {
	if err := s.store.Set(ctx, key, value, s.sessionTTL); err != nil {
		s.log.Warn("запись сессии", "user_id", userID, "error", err)
	}
}

func findChat(chats []housechat.Chat, id int64) (housechat.Chat, bool) {
	for _, c := range chats {
		if c.ID == id {
			return c, true
		}
	}
	return housechat.Chat{}, false
}

func sessionChatKey(userID int64) string {
	return fmt.Sprintf("session:%d:chat_id", userID)
}

func sessionGreetedKey(userID int64) string {
	return fmt.Sprintf("session:%d:greeted", userID)
}
