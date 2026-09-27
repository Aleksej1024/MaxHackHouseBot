package requests

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/usecase"
)

// ManageKind — результат действия автора со своей заявкой (раздел 8.7).
type ManageKind int

const (
	// ManageNoChat — нет активного чата: транспорт запускает сценарий первого сообщения.
	ManageNoChat ManageKind = iota + 1
	// ManageList — список своих заявок.
	ManageList
	// ManageCard — карточка заявки для автора.
	ManageCard
	// ManageNotFound — заявки нет, она чужая или удалена.
	ManageNotFound
	// ManageNotActive — заявка закрыта или истекла, действие недоступно.
	ManageNotActive
	// ManageAskResult — ждём текст итога для закрытия.
	ManageAskResult
	// ManageAskBody — ждём новый текст заявки.
	ManageAskBody
	// ManageConfirmDelete — спросить подтверждение удаления.
	ManageConfirmDelete
	ManageClosed
	ManageEdited
	ManageDeleted
	ManageCancelled
)

// ManageResult — что показать автору.
type ManageResult struct {
	Kind     ManageKind
	Chat     housechat.Chat
	Requests []request.Request
	Card     usecase.RequestCard
	// Problem — ошибка ввода (request.ErrEmptyBody, request.ErrBodyTooLong),
	// шаг показывается повторно.
	Problem error
	Limits  request.CreatePolicy
	// ChatSynced — удалось ли обновить сообщение в чате.
	ChatSynced bool
}

// RatingApplier начисляет рейтинг по итогам голосования (voting.Rater).
type RatingApplier interface {
	Apply(ctx context.Context, req request.Request) (int, error)
}

// ChatSyncer — синхронизация заявки с чатом (chatsync.Syncer).
type ChatSyncer interface {
	Sync(ctx context.Context, requestID int64) bool
	PublishResult(ctx context.Context, requestID int64) bool
}

// ManageDeps — зависимости сценария управления заявками.
type ManageDeps struct {
	Chats     ActiveChats
	Locker    usecase.RequestLocker
	Lifecycle usecase.RequestLifecycleRepo
	Cards     usecase.CardReader
	Rater     RatingApplier
	Sync      ChatSyncer
	Store     usecase.FSMStore
	Tx        usecase.TxManager
	Clock     usecase.Clock
	Log       *slog.Logger
}

// ManageConfig — параметры из конфига.
type ManageConfig struct {
	Policy    request.CreatePolicy // длина текста и итога
	FSMTTL    time.Duration
	ListLimit int // LIST_PAGE_SIZE
}

// Management — действия автора со своими заявками: список, карточка,
// правка, закрытие с итогом, удаление. Закрывает заявку только автор (п. 5a).
type Management struct {
	ManageDeps
	cfg ManageConfig
}

func NewManagement(deps ManageDeps, cfg ManageConfig) *Management {
	return &Management{ManageDeps: deps, cfg: cfg}
}

// MyRequests — «Мои заявки» в активном чате.
func (m *Management) MyRequests(ctx context.Context, userID int64) (ManageResult, error) {
	chat, ok, err := m.Chats.ActiveChat(ctx, userID)
	if err != nil {
		return ManageResult{}, err
	}
	if !ok {
		return m.result(ManageNoChat), nil
	}
	list, err := m.Lifecycle.ListByAuthor(ctx, chat.ID, userID, m.cfg.ListLimit)
	if err != nil {
		return ManageResult{}, fmt.Errorf("заявки автора: %w", err)
	}
	res := m.result(ManageList)
	res.Chat, res.Requests = chat, list
	return res, nil
}

// Open — карточка заявки для автора. Чужие и удалённые заявки не показываются.
func (m *Management) Open(ctx context.Context, requestID, userID int64) (ManageResult, error) {
	card, err := m.Cards.LoadCard(ctx, requestID)
	if errors.Is(err, usecase.ErrNotFound) {
		return m.result(ManageNotFound), nil
	}
	if err != nil {
		return ManageResult{}, fmt.Errorf("карточка заявки: %w", err)
	}
	if card.Request.AuthorID != userID || card.Request.Status == request.StatusDeleted {
		return m.result(ManageNotFound), nil
	}
	res := m.result(ManageCard)
	res.Card = card
	return res, nil
}

// StartClose — кнопка «Закрыть»: ждём текст итога.
func (m *Management) StartClose(ctx context.Context, requestID, userID int64) (ManageResult, error) {
	return m.startInput(ctx, requestID, userID, StateEnteringResult, ManageAskResult)
}

// StartEdit — кнопка «Изменить текст»: ждём новый текст.
func (m *Management) StartEdit(ctx context.Context, requestID, userID int64) (ManageResult, error) {
	return m.startInput(ctx, requestID, userID, StateEditingBody, ManageAskBody)
}

// AskDelete — кнопка «Удалить»: спросить подтверждение.
func (m *Management) AskDelete(ctx context.Context, requestID, userID int64) (ManageResult, error) {
	res, ok, err := m.checkActive(ctx, requestID, userID)
	if err != nil || !ok {
		return res, err
	}
	res.Kind = ManageConfirmDelete
	return res, nil
}

// Cancel — отмена ввода итога или текста.
func (m *Management) Cancel(ctx context.Context, userID int64) (ManageResult, error) {
	d, found, err := loadDraft(ctx, m.Store, m.Log, userID)
	if err != nil {
		return ManageResult{}, err
	}
	if found && d.State.isManagement() {
		dropDraft(ctx, m.Store, m.Log, userID)
	}
	return m.result(ManageCancelled), nil
}

// HandleMessage обрабатывает текст, если автор вводит итог или новый текст.
// handled = false, если такого диалога нет.
func (m *Management) HandleMessage(ctx context.Context, userID int64, text string) (ManageResult, bool, error) {
	d, found, err := loadDraft(ctx, m.Store, m.Log, userID)
	if err != nil || !found || !d.State.isManagement() {
		return ManageResult{}, false, err
	}
	text = strings.TrimSpace(text)

	if problem := m.cfg.Policy.CheckBody(text); problem != nil {
		// Продлеваем диалог и просим ввести заново.
		if err := saveDraft(ctx, m.Store, m.cfg.FSMTTL, userID, d); err != nil {
			return ManageResult{}, true, err
		}
		res := m.result(ManageAskResult)
		if d.State == StateEditingBody {
			res.Kind = ManageAskBody
		}
		res.Problem = problem
		return res, true, nil
	}

	var res ManageResult
	switch d.State {
	case StateEnteringResult:
		res, err = m.close(ctx, d.RequestID, userID, text)
	case StateEditingBody:
		res, err = m.edit(ctx, d.RequestID, userID, text)
	}
	if err != nil {
		return ManageResult{}, true, err
	}
	dropDraft(ctx, m.Store, m.Log, userID)
	return res, true, nil
}

// Delete — подтверждённое удаление: status = deleted, сообщение в чате
// удаляется, рейтинг не начисляется и не снимается.
func (m *Management) Delete(ctx context.Context, requestID, userID int64) (ManageResult, error) {
	var res ManageResult
	var req request.Request
	err := m.Tx.Do(ctx, func(ctx context.Context) error {
		var err error
		var ok bool
		if req, res, ok, err = m.lockOwn(ctx, requestID, userID); err != nil || !ok {
			return err
		}
		switch err := req.Delete(); {
		case errors.Is(err, request.ErrNotActive):
			res.Kind = ManageNotActive
			return nil
		case err != nil:
			return err
		}
		res.Kind = ManageDeleted
		return m.Lifecycle.SaveState(ctx, req)
	})
	if err != nil {
		return ManageResult{}, fmt.Errorf("удаление заявки %d: %w", requestID, err)
	}
	if res.Kind == ManageDeleted {
		res.ChatSynced = m.Sync.Sync(ctx, requestID)
		res.Card.Request = req
		m.Log.Info("заявка удалена автором", "request_id", requestID, "user_id", userID)
	}
	return res, nil
}

// close закрывает заявку с итогом. Из open голосование завершается
// досрочно, рейтинг считается в той же транзакции.
func (m *Management) close(ctx context.Context, requestID, userID int64, result string) (ManageResult, error) {
	var res ManageResult
	var req request.Request
	err := m.Tx.Do(ctx, func(ctx context.Context) error {
		var err error
		var ok bool
		if req, res, ok, err = m.lockOwn(ctx, requestID, userID); err != nil || !ok {
			return err
		}
		switch _, err := req.Close(result, m.Clock.Now()); {
		case errors.Is(err, request.ErrNotActive):
			res.Kind = ManageNotActive
			return nil
		case err != nil:
			return err
		}
		if err := m.Lifecycle.SaveState(ctx, req); err != nil {
			return err
		}
		if _, err := m.Rater.Apply(ctx, req); err != nil {
			return err
		}
		res.Kind = ManageClosed
		return nil
	})
	if err != nil {
		return ManageResult{}, fmt.Errorf("закрытие заявки %d: %w", requestID, err)
	}
	if res.Kind == ManageClosed {
		res.ChatSynced = m.Sync.Sync(ctx, requestID)
		m.Sync.PublishResult(ctx, requestID)
		res.Card.Request = req
		m.Log.Info("заявка закрыта автором", "request_id", requestID, "user_id", userID)
	}
	return res, nil
}

// edit меняет текст заявки; голоса не сбрасываются.
func (m *Management) edit(ctx context.Context, requestID, userID int64, body string) (ManageResult, error) {
	var res ManageResult
	var req request.Request
	err := m.Tx.Do(ctx, func(ctx context.Context) error {
		var err error
		var ok bool
		if req, res, ok, err = m.lockOwn(ctx, requestID, userID); err != nil || !ok {
			return err
		}
		if !req.IsActive() {
			res.Kind = ManageNotActive
			return nil
		}
		req.Body = body
		res.Kind = ManageEdited
		return m.Lifecycle.UpdateBody(ctx, requestID, body)
	})
	if err != nil {
		return ManageResult{}, fmt.Errorf("правка заявки %d: %w", requestID, err)
	}
	if res.Kind == ManageEdited {
		res.ChatSynced = m.Sync.Sync(ctx, requestID)
		res.Card.Request = req
		m.Log.Info("заявка изменена автором", "request_id", requestID, "user_id", userID)
	}
	return res, nil
}

// startInput проверяет права и переводит диалог в ожидание текста.
func (m *Management) startInput(ctx context.Context, requestID, userID int64, state State, kind ManageKind) (ManageResult, error) {
	res, ok, err := m.checkActive(ctx, requestID, userID)
	if err != nil || !ok {
		return res, err
	}
	d := Draft{State: state, RequestID: requestID}
	if err := saveDraft(ctx, m.Store, m.cfg.FSMTTL, userID, d); err != nil {
		return ManageResult{}, err
	}
	res.Kind = kind
	return res, nil
}

// checkActive — без блокировки: заявка своя и ещё активна. Окончательная
// проверка повторяется под блокировкой при выполнении действия.
func (m *Management) checkActive(ctx context.Context, requestID, userID int64) (ManageResult, bool, error) {
	res, err := m.Open(ctx, requestID, userID)
	if err != nil || res.Kind != ManageCard {
		return res, false, err
	}
	if !res.Card.Request.IsActive() {
		res.Kind = ManageNotActive
		return res, false, nil
	}
	return res, true, nil
}

// lockOwn блокирует заявку и проверяет авторство. ok = false — res содержит отказ.
func (m *Management) lockOwn(ctx context.Context, requestID, userID int64) (request.Request, ManageResult, bool, error) {
	req, err := m.Locker.GetForUpdate(ctx, requestID)
	if errors.Is(err, usecase.ErrNotFound) {
		return req, m.result(ManageNotFound), false, nil
	}
	if err != nil {
		return req, ManageResult{}, false, err
	}
	if req.AuthorID != userID || req.Status == request.StatusDeleted {
		return req, m.result(ManageNotFound), false, nil
	}
	return req, m.result(0), true, nil
}

func (m *Management) result(kind ManageKind) ManageResult {
	return ManageResult{Kind: kind, Limits: m.cfg.Policy}
}
