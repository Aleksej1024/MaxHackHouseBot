package requests

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/usecase"
)

// State — состояние диалога в личке. Все диалоги сценариев заявок хранятся
// под одним ключом fsm:{user_id}: начало нового диалога прерывает прежний.
type State string

// Состояния создания заявки (раздел 8.2).
const (
	StateChoosingType      State = "choosing_type"
	StateEnteringText      State = "entering_text"
	StateAddingMedia       State = "adding_media"
	StateChoosingAnonymity State = "choosing_anonymity"
	StateConfirming        State = "confirming"
)

// Состояния управления заявкой автором (раздел 8.7).
const (
	StateEnteringResult State = "entering_result" // ввод итога при закрытии
	StateEditingBody    State = "editing_body"    // ввод нового текста
)

// Состояния материалов жителя к заявке (раздел 8.4).
const (
	StateWritingComment State = "writing_comment"
	StateAddingEvidence State = "adding_evidence"
)

// Состояния рассылки автора (раздел 8.5).
const (
	StateBroadcastText    State = "broadcast_text"
	StateBroadcastConfirm State = "broadcast_confirm"
)

func (s State) isMaterial() bool {
	return s == StateWritingComment || s == StateAddingEvidence
}

func (s State) isBroadcast() bool {
	return s == StateBroadcastText || s == StateBroadcastConfirm
}

// Flow — какой сценарий ведёт диалог в личке.
type Flow int

const (
	FlowNone Flow = iota
	FlowCreate
	FlowManage
	FlowMaterial
	FlowBroadcast
)

func (s State) flow() Flow {
	switch {
	case s.isCreation():
		return FlowCreate
	case s.isManagement():
		return FlowManage
	case s.isMaterial():
		return FlowMaterial
	case s.isBroadcast():
		return FlowBroadcast
	}
	return FlowNone
}

// Dialogs определяет текущий диалог пользователя, чтобы транспорт
// передал сообщение нужному сценарию.
type Dialogs struct {
	store usecase.FSMStore
	log   *slog.Logger
}

func NewDialogs(store usecase.FSMStore, log *slog.Logger) *Dialogs {
	return &Dialogs{store: store, log: log}
}

// Reset прерывает текущий диалог пользователя (кнопка «В меню»): черновик
// создания, ввод итога, материала или рассылки удаляется, как по «Отмене».
func (d *Dialogs) Reset(ctx context.Context, userID int64) error {
	if err := d.store.Delete(ctx, fsmKey(userID)); err != nil {
		return fmt.Errorf("сброс диалога: %w", err)
	}
	return nil
}

// Current возвращает сценарий текущего диалога (FlowNone — диалога нет).
func (d *Dialogs) Current(ctx context.Context, userID int64) (Flow, error) {
	draft, found, err := loadDraft(ctx, d.store, d.log, userID)
	if err != nil || !found {
		return FlowNone, err
	}
	return draft.State.flow(), nil
}

func (s State) isCreation() bool {
	switch s {
	case StateChoosingType, StateEnteringText, StateAddingMedia, StateChoosingAnonymity, StateConfirming:
		return true
	}
	return false
}

func (s State) isManagement() bool {
	return s == StateEnteringResult || s == StateEditingBody
}

// Draft — состояние диалога и черновик в FSMStore.
type Draft struct {
	State       State                `json:"state"`
	ChatID      int64                `json:"chat_id,omitempty"`
	RequestID   int64                `json:"request_id,omitempty"`
	Type        requesttype.Code     `json:"type,omitempty"`
	Body        string               `json:"body,omitempty"`
	Attachments []request.Attachment `json:"attachments,omitempty"`
	Anonymous   bool                 `json:"anonymous"`
	Audience    string               `json:"audience,omitempty"`
}

func fsmKey(userID int64) string {
	return fmt.Sprintf("fsm:%d", userID)
}

// loadDraft читает диалог пользователя. Повреждённый черновик равносилен
// устаревшему.
func loadDraft(ctx context.Context, store usecase.FSMStore, log *slog.Logger, userID int64) (Draft, bool, error) {
	raw, found, err := store.Get(ctx, fsmKey(userID))
	if err != nil {
		return Draft{}, false, fmt.Errorf("чтение черновика: %w", err)
	}
	if !found {
		return Draft{}, false, nil
	}
	var d Draft
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		log.Warn("повреждённый черновик", "user_id", userID, "error", err)
		return Draft{}, false, nil
	}
	return d, true, nil
}

// saveDraft записывает диалог и продлевает TTL.
func saveDraft(ctx context.Context, store usecase.FSMStore, ttl time.Duration, userID int64, d Draft) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("сериализация черновика: %w", err)
	}
	if err := store.Set(ctx, fsmKey(userID), string(raw), ttl); err != nil {
		return fmt.Errorf("запись черновика: %w", err)
	}
	return nil
}

func dropDraft(ctx context.Context, store usecase.FSMStore, log *slog.Logger, userID int64) {
	if err := store.Delete(ctx, fsmKey(userID)); err != nil {
		log.Error("удаление черновика", "user_id", userID, "error", err)
	}
}
