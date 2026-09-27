// Package requests — сценарии работы с заявками.
//
// create.go — создание заявки в личке (раздел 8.2). Состояния и переходы
// конечного автомата — здесь, черновик хранится в FSMStore под ключом
// fsm:{user_id} с TTL FSM_TTL, продлеваемым на каждом шаге.
// Use case не содержит текстов: транспорт отображает Step.
package requests

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/usecase"
	"maxhouse/internal/usecase/notify"
)

// StepKind — что показать пользователю после действия.
type StepKind int

const (
	// StepNoChat — нет активного чата: транспорт запускает сценарий первого сообщения.
	StepNoChat StepKind = iota + 1
	StepRatingTooLow
	StepLimitReached
	StepChooseType
	StepEnterText
	StepAddMedia
	StepChooseAnonymity
	StepConfirm
	StepPublished
	StepCancelled
	// StepExpired — черновик устарел (истёк FSM_TTL), нужно начать заново.
	StepExpired
)

// ErrUnexpectedInput — действие не подходит к текущему шагу
// (например, текст вместо нажатия кнопки или старая кнопка).
var ErrUnexpectedInput = errors.New("действие не подходит к текущему шагу")

// Step — результат действия пользователя.
type Step struct {
	Kind  StepKind
	Draft Draft
	// Types — доступные типы (для StepChooseType).
	Types []requesttype.Type
	// Problem — почему шаг показан повторно: request.ErrEmptyBody,
	// request.ErrBodyTooLong, request.ErrTooManyMedia, ErrUnexpectedInput.
	Problem error
	// Limits — действующие ограничения (для подсказок с числами из конфига).
	Limits request.CreatePolicy
	// Request и ChatSynced заполняются для StepPublished.
	Request    request.Request
	ChatSynced bool
}

// ActiveChats — выбор активного чата пользователя (реализует onboarding.Service).
type ActiveChats interface {
	ActiveChat(ctx context.Context, userID int64) (housechat.Chat, bool, error)
}

// Forwarder пересылает новую заявку жителям с включённой пересылкой
// (notify.Notifier).
type Forwarder interface {
	ForwardNew(ctx context.Context, requestID int64) notify.Delivery
}

// Config — параметры сценария из конфига.
type Config struct {
	Policy         request.CreatePolicy
	FSMTTL         time.Duration
	VotingDuration time.Duration
	TTLDefault     time.Duration
}

// Deps — зависимости сценария.
type Deps struct {
	Chats     ActiveChats
	Members   usecase.MembershipReader
	Requests  usecase.RequestRepo
	Publisher usecase.ChatPublisher
	Forward   Forwarder
	Store     usecase.FSMStore
	Tx        usecase.TxManager
	Clock     usecase.Clock
	Types     *requesttype.Registry
	Log       *slog.Logger
}

// Creation реализует сценарий создания заявки.
type Creation struct {
	Deps
	cfg Config
}

func NewCreation(deps Deps, cfg Config) *Creation {
	return &Creation{Deps: deps, cfg: cfg}
}

// Begin — кнопка «Создать заявку». Лимит и порог рейтинга проверяются
// до входа в диалог (и повторно при публикации).
func (c *Creation) Begin(ctx context.Context, userID int64) (Step, error) {
	chat, ok, err := c.Chats.ActiveChat(ctx, userID)
	if err != nil {
		return Step{}, err
	}
	if !ok {
		return c.step(StepNoChat, Draft{}), nil
	}

	var kind StepKind
	err = c.Tx.Do(ctx, func(ctx context.Context) error {
		var err error
		// Тип ещё не выбран: здесь только порог рейтинга, лимит — при выборе типа.
		kind, err = c.checkCanCreate(ctx, chat.ID, userID, "")
		return err
	})
	if err != nil {
		return Step{}, err
	}
	if kind != 0 {
		return c.step(kind, Draft{}), nil
	}

	d := Draft{State: StateChoosingType, ChatID: chat.ID}
	if err := c.save(ctx, userID, d); err != nil {
		return Step{}, err
	}
	return c.current(d), nil
}

// ChooseType — выбор типа заявки кнопкой. Суточный лимит действует на каждый
// тип отдельно: если по этому типу он исчерпан, шаг выбора показывается снова
// (ErrDailyLimit), другой тип выбрать можно.
func (c *Creation) ChooseType(ctx context.Context, userID int64, code requesttype.Code) (Step, error) {
	if _, known := c.Types.Get(code); known {
		d, found, err := c.load(ctx, userID)
		if err != nil {
			return Step{}, err
		}
		if found && d.State == StateChoosingType {
			var kind StepKind
			err := c.Tx.Do(ctx, func(ctx context.Context) error {
				var err error
				kind, err = c.checkCanCreate(ctx, d.ChatID, userID, code)
				return err
			})
			if err != nil {
				return Step{}, err
			}
			switch kind {
			case 0:
			case StepLimitReached:
				if err := c.save(ctx, userID, d); err != nil {
					return Step{}, err
				}
				s := c.current(d)
				s.Problem = request.ErrDailyLimit
				return s, nil
			default:
				c.dropDraft(ctx, userID)
				return c.step(kind, Draft{}), nil
			}
		}
	}
	return c.transition(ctx, userID, StateChoosingType, func(d *Draft) error {
		if _, ok := c.Types.Get(code); !ok {
			return ErrUnexpectedInput
		}
		d.Type = code
		d.State = StateEnteringText
		return nil
	})
}

// HandleMessage обрабатывает сообщение в личке, если идёт диалог создания.
// handled = false, если черновика нет: сообщение обрабатывает меню.
func (c *Creation) HandleMessage(ctx context.Context, userID int64, text string, atts []request.Attachment) (step Step, handled bool, err error) {
	d, found, err := c.load(ctx, userID)
	if err != nil || !found {
		return Step{}, false, err
	}
	text = strings.TrimSpace(text)

	var problem error
	switch d.State {
	case StateEnteringText:
		problem = c.cfg.Policy.CheckBody(text)
		if problem == nil {
			problem = c.cfg.Policy.CheckAttachments(len(d.Attachments) + len(atts))
		}
		if problem == nil {
			d.Body = text
			d.Attachments = append(d.Attachments, atts...)
			d.State = StateAddingMedia
		}
	case StateAddingMedia:
		switch {
		case len(atts) == 0:
			problem = ErrUnexpectedInput
		default:
			problem = c.cfg.Policy.CheckAttachments(len(d.Attachments) + len(atts))
			if problem == nil {
				d.Attachments = append(d.Attachments, atts...)
			}
		}
	default:
		problem = ErrUnexpectedInput
	}

	// Сохраняем и при ошибке ввода: продлеваем TTL черновика.
	if err := c.save(ctx, userID, d); err != nil {
		return Step{}, true, err
	}
	s := c.current(d)
	s.Problem = problem
	return s, true, nil
}

// FinishMedia — кнопка «Готово» на шаге вложений.
func (c *Creation) FinishMedia(ctx context.Context, userID int64) (Step, error) {
	return c.transition(ctx, userID, StateAddingMedia, func(d *Draft) error {
		d.State = StateChoosingAnonymity
		return nil
	})
}

// ChooseAnonymity — выбор, показывать ли автора жителям.
func (c *Creation) ChooseAnonymity(ctx context.Context, userID int64, anonymous bool) (Step, error) {
	return c.transition(ctx, userID, StateChoosingAnonymity, func(d *Draft) error {
		d.Anonymous = anonymous
		d.State = StateConfirming
		return nil
	})
}

// Edit — кнопка «Изменить» на подтверждении: возврат к вводу текста.
// Приложенные вложения сохраняются.
func (c *Creation) Edit(ctx context.Context, userID int64) (Step, error) {
	return c.transition(ctx, userID, StateConfirming, func(d *Draft) error {
		d.State = StateEnteringText
		return nil
	})
}

// Cancel — кнопка «Отмена» на любом шаге.
func (c *Creation) Cancel(ctx context.Context, userID int64) (Step, error) {
	if err := c.Store.Delete(ctx, fsmKey(userID)); err != nil {
		return Step{}, fmt.Errorf("удаление черновика: %w", err)
	}
	return c.step(StepCancelled, Draft{}), nil
}

// Publish — кнопка «Опубликовать». Схема раздела 17, п. 4: транзакция
// в Postgres (повторная проверка прав и лимитов, INSERT), затем сообщение
// в чат; при ошибке MAX — chat_sync = failed, данные не откатываются.
func (c *Creation) Publish(ctx context.Context, u membership.User) (Step, error) {
	d, found, err := c.load(ctx, u.ID)
	if err != nil {
		return Step{}, err
	}
	if !found {
		return c.step(StepExpired, Draft{}), nil
	}
	if d.State != StateConfirming {
		s := c.current(d)
		s.Problem = ErrUnexpectedInput
		return s, nil
	}
	tp, ok := c.Types.Get(d.Type)
	if !ok {
		s := c.current(d)
		s.Problem = ErrUnexpectedInput
		return s, nil
	}

	now := c.Clock.Now()
	voting, ttl := tp.Durations(c.cfg.VotingDuration, c.cfg.TTLDefault)
	votingEnds, expires := request.Deadlines(now, voting, ttl)
	req := request.Request{
		ChatID:       d.ChatID,
		AuthorID:     u.ID,
		Type:         d.Type,
		Status:       request.StatusOpen,
		IsAnonymous:  d.Anonymous,
		Body:         d.Body,
		CreatedAt:    now,
		VotingEndsAt: votingEnds,
		ExpiresAt:    expires,
	}

	var kind StepKind
	err = c.Tx.Do(ctx, func(ctx context.Context) error {
		var err error
		if kind, err = c.checkCanCreate(ctx, d.ChatID, u.ID, d.Type); err != nil || kind != 0 {
			return err
		}
		req.ID, err = c.Requests.Create(ctx, req, d.Attachments)
		return err
	})
	if err != nil {
		return Step{}, fmt.Errorf("сохранение заявки: %w", err)
	}
	if kind != 0 {
		c.dropDraft(ctx, u.ID)
		// Черновик в шаге — для текста отказа (тип заявки при исчерпанном лимите).
		return c.step(kind, d), nil
	}
	c.dropDraft(ctx, u.ID)

	card := usecase.RequestCard{Request: req, Attachments: d.Attachments}
	if !req.IsAnonymous {
		card.AuthorNickname = u.Nickname
	}
	synced := c.publish(ctx, &req, card)
	c.Forward.ForwardNew(ctx, req.ID)

	c.Log.Info("заявка создана",
		"request_id", req.ID, "chat_id", req.ChatID, "user_id", u.ID,
		"type", string(req.Type), "chat_synced", synced)

	s := c.step(StepPublished, d)
	s.Request = req
	s.ChatSynced = synced
	return s, nil
}

// publish отправляет карточку в чат и сохраняет результат синхронизации.
func (c *Creation) publish(ctx context.Context, req *request.Request, card usecase.RequestCard) bool {
	attrs := []any{"request_id", req.ID, "chat_id", req.ChatID}
	mid, err := c.Publisher.PublishRequest(ctx, card)
	if err != nil {
		c.Log.Error("публикация заявки в чате", append(attrs, "error", err)...)
		if err := c.Requests.SetChatSyncFailed(ctx, req.ID); err != nil {
			c.Log.Error("отметка chat_sync = failed", append(attrs, "error", err)...)
		}
		return false
	}
	req.ChatMessageID = mid
	if err := c.Requests.SetChatMessage(ctx, req.ID, mid); err != nil {
		c.Log.Error("сохранение chat_message_id", append(attrs, "error", err)...)
	}
	return true
}

// checkCanCreate проверяет участие, порог рейтинга и суточный лимит типа typ
// (пустой typ — тип ещё не выбран, лимит не проверяется).
// Вызывается внутри транзакции: строка участия блокируется до её конца.
// Возвращает 0, если создавать можно, иначе вид шага с отказом.
func (c *Creation) checkCanCreate(ctx context.Context, chatID, userID int64, typ requesttype.Code) (StepKind, error) {
	m, err := c.Members.GetActiveForUpdate(ctx, chatID, userID)
	if errors.Is(err, usecase.ErrNotFound) {
		return StepNoChat, nil
	}
	if err != nil {
		return 0, fmt.Errorf("участие пользователя: %w", err)
	}
	if typ == "" {
		if errors.Is(c.cfg.Policy.CheckRating(m.Rating), request.ErrRatingTooLow) {
			return StepRatingTooLow, nil
		}
		return 0, nil
	}
	since := c.Clock.Now().Add(-request.LimitWindow)
	n, err := c.Requests.CountCreatedSince(ctx, chatID, userID, typ, since)
	if err != nil {
		return 0, fmt.Errorf("подсчёт заявок: %w", err)
	}
	switch err := c.cfg.Policy.CheckCanCreate(m.Rating, n); {
	case errors.Is(err, request.ErrRatingTooLow):
		return StepRatingTooLow, nil
	case errors.Is(err, request.ErrDailyLimit):
		return StepLimitReached, nil
	}
	return 0, nil
}

// transition выполняет переход из состояния from. Если черновика нет —
// StepExpired; если состояние другое (старая кнопка) — текущий шаг
// с ErrUnexpectedInput.
func (c *Creation) transition(ctx context.Context, userID int64, from State, apply func(d *Draft) error) (Step, error) {
	d, found, err := c.load(ctx, userID)
	if err != nil {
		return Step{}, err
	}
	if !found {
		return c.step(StepExpired, Draft{}), nil
	}
	var problem error
	if d.State != from {
		problem = ErrUnexpectedInput
	} else {
		problem = apply(&d)
	}
	if err := c.save(ctx, userID, d); err != nil {
		return Step{}, err
	}
	s := c.current(d)
	s.Problem = problem
	return s, nil
}

// current — шаг, соответствующий состоянию черновика.
func (c *Creation) current(d Draft) Step {
	var kind StepKind
	switch d.State {
	case StateChoosingType:
		kind = StepChooseType
	case StateEnteringText:
		kind = StepEnterText
	case StateAddingMedia:
		kind = StepAddMedia
	case StateChoosingAnonymity:
		kind = StepChooseAnonymity
	case StateConfirming:
		kind = StepConfirm
	default:
		kind = StepExpired
	}
	return c.step(kind, d)
}

func (c *Creation) step(kind StepKind, d Draft) Step {
	s := Step{Kind: kind, Draft: d, Limits: c.cfg.Policy}
	if kind == StepChooseType {
		s.Types = c.Types.All()
	}
	return s
}

// load читает черновик создания. Диалог другого сценария (закрытие,
// правка) для создания равносилен отсутствию черновика.
func (c *Creation) load(ctx context.Context, userID int64) (Draft, bool, error) {
	d, found, err := loadDraft(ctx, c.Store, c.Log, userID)
	if err != nil || !found || !d.State.isCreation() {
		return Draft{}, false, err
	}
	return d, true, nil
}

func (c *Creation) save(ctx context.Context, userID int64, d Draft) error {
	return saveDraft(ctx, c.Store, c.cfg.FSMTTL, userID, d)
}

func (c *Creation) dropDraft(ctx context.Context, userID int64) {
	dropDraft(ctx, c.Store, c.Log, userID)
}
