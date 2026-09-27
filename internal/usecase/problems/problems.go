// Package problems — сообщения жителей о бытовых проблемах и оповещение
// дома о массовой проблеме.
//
// Житель выбирает проблему из списка в личке. Сообщения копятся в окне
// PROBLEM_ALERT_WINDOW с первого сообщения; когда в окне набирается
// PROBLEM_ALERT_THRESHOLD разных жителей, в домовой чат один раз уходит
// оповещение с упоминанием администраторов. Кто сообщил, соседи не видят.
package problems

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/problem"
	"maxhouse/internal/usecase"
)

// Outcome — итог сообщения о проблеме.
type Outcome int

const (
	// OutcomeNoChat — нет активного чата: транспорт запускает первое сообщение.
	OutcomeNoChat Outcome = iota + 1
	// OutcomeList — список проблем для выбора.
	OutcomeList
	// OutcomeRecorded — сообщение учтено.
	OutcomeRecorded
	// OutcomeAlreadyReported — житель уже сообщал об этой проблеме в этом окне.
	OutcomeAlreadyReported
	// OutcomeUnknown — неизвестный код проблемы (старая или чужая кнопка).
	OutcomeUnknown
)

// Result — что показать жителю.
type Result struct {
	Outcome  Outcome
	Problems []problem.Code
	Problem  problem.Code
	// Alerted — это сообщение набрало порог, оповещение ушло в чат.
	Alerted bool
}

// ActiveChats — активный чат жителя (onboarding.Service).
type ActiveChats interface {
	ActiveChat(ctx context.Context, userID int64) (housechat.Chat, bool, error)
}

// Deps — зависимости сценария.
type Deps struct {
	Chats   ActiveChats
	Repo    usecase.ProblemRepo
	Members usecase.MemberLister // свежий список участников из MAX (признак админа)
	Admins  usecase.ChatAdmins   // запасной список админов из БД
	Alerter usecase.ProblemAlerter
	Tx      usecase.TxManager
	Clock   usecase.Clock
	Log     *slog.Logger
}

// Service реализует сценарий.
type Service struct {
	Deps
	policy problem.Policy
}

func New(deps Deps, policy problem.Policy) *Service {
	return &Service{Deps: deps, policy: policy}
}

// List — кнопка «Сообщить о проблеме»: список проблем для активного дома.
func (s *Service) List(ctx context.Context, userID int64) (Result, error) {
	_, ok, err := s.Chats.ActiveChat(ctx, userID)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{Outcome: OutcomeNoChat}, nil
	}
	return Result{Outcome: OutcomeList, Problems: problem.Codes}, nil
}

// Report — житель выбрал проблему. В транзакции под блокировкой проблемы
// дома: найти открытое окно или открыть новое, добавить жителя, решить,
// пора ли оповещать. Оповещение отправляется после коммита; если отправить
// не удалось, отметка снимается и следующее сообщение попробует снова.
func (s *Service) Report(ctx context.Context, userID int64, code problem.Code) (Result, error) {
	if !problem.Known(code) {
		return Result{Outcome: OutcomeUnknown}, nil
	}
	chat, ok, err := s.Chats.ActiveChat(ctx, userID)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{Outcome: OutcomeNoChat}, nil
	}

	now := s.Clock.Now()
	res := Result{Outcome: OutcomeRecorded, Problem: code}
	var incident problem.Incident
	err = s.Tx.Do(ctx, func(ctx context.Context) error {
		if err := s.Repo.Lock(ctx, chat.ID, code); err != nil {
			return err
		}
		var found bool
		var err error
		incident, found, err = s.Repo.Current(ctx, chat.ID, code, s.policy.OpenSince(now))
		if err != nil {
			return err
		}
		if !found {
			incident = problem.New(chat.ID, code, now)
			if incident.ID, err = s.Repo.Create(ctx, incident); err != nil {
				return err
			}
		}
		added, err := s.Repo.AddReporter(ctx, incident.ID, userID, now)
		if err != nil {
			return err
		}
		if !added {
			res.Outcome = OutcomeAlreadyReported
			return nil
		}
		incident.AddReporter()
		if !incident.NeedsAlert(s.policy) {
			return nil
		}
		incident.MarkNotified(now)
		res.Alerted = true
		return s.Repo.SetNotified(ctx, incident.ID, now)
	})
	if err != nil {
		return Result{}, fmt.Errorf("сообщение о проблеме: %w", err)
	}
	s.Log.Info("сообщение о проблеме", "chat_id", chat.ID, "user_id", userID,
		"problem", string(code), "reporters", incident.Reporters, "alerted", res.Alerted)

	if res.Alerted && !s.alert(ctx, incident) {
		res.Alerted = false
	}
	return res, nil
}

// alert отправляет оповещение. При ошибке снимает отметку, чтобы следующее
// сообщение о проблеме в этом окне повторило попытку.
func (s *Service) alert(ctx context.Context, incident problem.Incident) bool {
	attrs := []any{"chat_id", incident.ChatID, "problem", string(incident.Problem), "incident_id", incident.ID}
	admins := s.admins(ctx, incident.ChatID)
	if err := s.Alerter.SendProblemAlert(ctx, incident.ChatID, incident.Problem, admins); err != nil {
		s.Log.Error("оповещение о массовой проблеме не отправлено", append(attrs, "error", err)...)
		if err := s.Repo.SetNotified(ctx, incident.ID, time.Time{}); err != nil {
			s.Log.Error("снятие отметки об оповещении", append(attrs, "error", err)...)
		}
		return false
	}
	s.Log.Info("оповещение о массовой проблеме отправлено", append(attrs, "admins", len(admins))...)
	return true
}

// admins — администраторы чата для упоминания: свежий список из MAX
// (роли могли измениться после добавления бота), при ошибке — из БД.
func (s *Service) admins(ctx context.Context, chatID int64) []membership.User {
	members, err := s.Members.ListMembers(ctx, chatID)
	if err == nil {
		var admins []membership.User
		for _, m := range members {
			if m.IsAdmin && !m.IsBot {
				admins = append(admins, membership.User{ID: m.UserID, Nickname: m.Nickname})
			}
		}
		return admins
	}
	s.Log.Warn("список участников из MAX недоступен, админы — из БД", "chat_id", chatID, "error", err)
	admins, err := s.Admins.ListChatAdmins(ctx, chatID)
	if err != nil {
		s.Log.Error("список админов чата", "chat_id", chatID, "error", err)
		return nil
	}
	return admins
}
