package requests

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"maxhouse/internal/domain/comment"
	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/usecase"
)

// MaterialKind — результат действий жителя с заявкой в личке (раздел 8.4).
type MaterialKind int

const (
	MatNoChat MaterialKind = iota + 1
	// MatHouseList — активные заявки дома.
	MatHouseList
	// MatAuthorCard — заявку открыл её автор: показываем карточку автора.
	MatAuthorCard
	// MatNeighborCard — карточка для соседа.
	MatNeighborCard
	MatNotFound
	MatNotActive
	MatOwnRequest
	MatAskComment
	MatAskEvidence
	MatAdded
	// MatMaterials — страница материалов для автора.
	MatMaterials
	MatCancelled
)

// MaterialResult — что показать жителю.
type MaterialResult struct {
	Kind     MaterialKind
	Chat     housechat.Chat
	Requests []request.Request
	Card     usecase.RequestCard
	// CanContribute — сосед может добавить комментарий или доказательство.
	CanContribute bool
	// Comments, Offset, Total — страница материалов (MatMaterials).
	Comments []comment.Comment
	Offset   int
	Total    int
	// Added — сохранённый материал (MatAdded).
	Added   comment.Comment
	Problem error
	Limits  request.CreatePolicy
}

// NextOffset — смещение следующей страницы материалов; 0 — страниц больше нет.
func (r MaterialResult) NextOffset() int {
	if next := r.Offset + len(r.Comments); next < r.Total {
		return next
	}
	return 0
}

// MaterialNotifier уведомляет автора о материале (notify.Notifier).
type MaterialNotifier interface {
	MaterialAdded(ctx context.Context, req request.Request, c comment.Comment) bool
}

// MaterialDeps — зависимости сценария.
type MaterialDeps struct {
	Chats    ActiveChats
	Browser  usecase.RequestBrowser
	Cards    usecase.CardReader
	Members  usecase.MembershipReader
	Locker   usecase.RequestLocker
	Comments usecase.CommentRepo
	Sync     ChatSyncer
	Notify   MaterialNotifier
	Store    usecase.FSMStore
	Tx       usecase.TxManager
	Clock    usecase.Clock
	Log      *slog.Logger
}

// MaterialConfig — параметры из конфига.
type MaterialConfig struct {
	Policy   request.CreatePolicy // длина текста и число вложений
	FSMTTL   time.Duration
	PageSize int // LIST_PAGE_SIZE
}

// Materials — просмотр заявок жителями, комментарии и доказательства.
// Материалы видит только автор заявки (раздел 8.4).
type Materials struct {
	MaterialDeps
	cfg MaterialConfig
}

func NewMaterials(deps MaterialDeps, cfg MaterialConfig) *Materials {
	return &Materials{MaterialDeps: deps, cfg: cfg}
}

// HouseRequests — «Заявки дома»: открытые заявки активного чата.
func (m *Materials) HouseRequests(ctx context.Context, userID int64) (MaterialResult, error) {
	chat, ok, err := m.Chats.ActiveChat(ctx, userID)
	if err != nil {
		return MaterialResult{}, err
	}
	if !ok {
		return m.result(MatNoChat), nil
	}
	list, err := m.Browser.ListActiveInChat(ctx, chat.ID, m.cfg.PageSize)
	if err != nil {
		return MaterialResult{}, fmt.Errorf("заявки дома: %w", err)
	}
	res := m.result(MatHouseList)
	res.Chat, res.Requests = chat, list
	return res, nil
}

// View открывает заявку по номеру или кнопке из чата. Заявку видят только
// активные участники её чата; автору показывается карточка автора.
func (m *Materials) View(ctx context.Context, requestID, userID int64) (MaterialResult, error) {
	card, err := m.Cards.LoadCard(ctx, requestID)
	if errors.Is(err, usecase.ErrNotFound) {
		return m.result(MatNotFound), nil
	}
	if err != nil {
		return MaterialResult{}, fmt.Errorf("карточка заявки: %w", err)
	}
	if card.Request.Status == request.StatusDeleted {
		return m.result(MatNotFound), nil
	}
	res := m.result(MatNeighborCard)
	res.Card = card
	if card.Request.AuthorID == userID {
		res.Kind = MatAuthorCard
		return res, nil
	}
	if _, err := m.Members.GetActiveForUpdate(ctx, card.Request.ChatID, userID); err != nil {
		if errors.Is(err, usecase.ErrNotFound) {
			return m.result(MatNotFound), nil
		}
		return MaterialResult{}, fmt.Errorf("участие пользователя: %w", err)
	}
	res.CanContribute = comment.CheckCanAdd(card.Request, userID) == nil
	return res, nil
}

// StartComment — кнопка «Оставить комментарий».
func (m *Materials) StartComment(ctx context.Context, requestID, userID int64) (MaterialResult, error) {
	return m.startInput(ctx, requestID, userID, StateWritingComment, MatAskComment)
}

// StartEvidence — кнопка «Добавить доказательство».
func (m *Materials) StartEvidence(ctx context.Context, requestID, userID int64) (MaterialResult, error) {
	return m.startInput(ctx, requestID, userID, StateAddingEvidence, MatAskEvidence)
}

// Cancel — отмена ввода материала.
func (m *Materials) Cancel(ctx context.Context, userID int64) (MaterialResult, error) {
	d, found, err := loadDraft(ctx, m.Store, m.Log, userID)
	if err != nil {
		return MaterialResult{}, err
	}
	if found && d.State.isMaterial() {
		dropDraft(ctx, m.Store, m.Log, userID)
	}
	return m.result(MatCancelled), nil
}

// HandleMessage сохраняет комментарий или доказательство, если житель
// в этом диалоге. handled = false, если диалога нет.
func (m *Materials) HandleMessage(ctx context.Context, u membership.User, text string, atts []request.Attachment) (MaterialResult, bool, error) {
	d, found, err := loadDraft(ctx, m.Store, m.Log, u.ID)
	if err != nil || !found || !d.State.isMaterial() {
		return MaterialResult{}, false, err
	}
	kind, ask := comment.KindComment, MatAskComment
	if d.State == StateAddingEvidence {
		kind, ask = comment.KindEvidence, MatAskEvidence
	}
	text = strings.TrimSpace(text)

	if problem := comment.Validate(kind, text, len(atts), m.cfg.Policy); problem != nil {
		if err := saveDraft(ctx, m.Store, m.cfg.FSMTTL, u.ID, d); err != nil {
			return MaterialResult{}, true, err
		}
		res := m.result(ask)
		res.Card.Request.ID = d.RequestID
		res.Problem = problem
		return res, true, nil
	}

	c := comment.Comment{
		RequestID:      d.RequestID,
		AuthorID:       u.ID,
		AuthorNickname: u.Nickname,
		Kind:           kind,
		Body:           text,
		Attachments:    atts,
		CreatedAt:      m.Clock.Now(),
	}
	var req request.Request
	var refusal MaterialKind
	err = m.Tx.Do(ctx, func(ctx context.Context) error {
		var err error
		req, err = m.Locker.GetForUpdate(ctx, d.RequestID)
		if errors.Is(err, usecase.ErrNotFound) || (err == nil && req.Status == request.StatusDeleted) {
			refusal = MatNotFound
			return nil
		}
		if err != nil {
			return err
		}
		switch err := comment.CheckCanAdd(req, u.ID); {
		case errors.Is(err, request.ErrNotActive):
			refusal = MatNotActive
			return nil
		case errors.Is(err, comment.ErrOwnRequest):
			refusal = MatOwnRequest
			return nil
		case err != nil:
			return err
		}
		if _, err := m.Members.GetActiveForUpdate(ctx, req.ChatID, u.ID); err != nil {
			if errors.Is(err, usecase.ErrNotFound) {
				refusal = MatNotFound
				return nil
			}
			return err
		}
		c.ID, err = m.Comments.Add(ctx, c)
		return err
	})
	if err != nil {
		return MaterialResult{}, true, fmt.Errorf("сохранение материала к заявке %d: %w", d.RequestID, err)
	}
	dropDraft(ctx, m.Store, m.Log, u.ID)
	if refusal != 0 {
		return m.result(refusal), true, nil
	}

	m.Log.Info("материал добавлен", "request_id", req.ID, "user_id", u.ID, "kind", string(kind))
	// Счётчик материалов в карточке чата и уведомление автору.
	m.Sync.Sync(ctx, req.ID)
	m.Notify.MaterialAdded(ctx, req, c)

	res := m.result(MatAdded)
	res.Card.Request = req
	res.Added = c
	return res, true, nil
}

// ListMaterials — раздел «Комментарии и материалы» в карточке автора:
// страница материалов с ником отправителя. Доступно только автору.
func (m *Materials) ListMaterials(ctx context.Context, requestID, userID int64, offset int) (MaterialResult, error) {
	card, err := m.Cards.LoadCard(ctx, requestID)
	if errors.Is(err, usecase.ErrNotFound) {
		return m.result(MatNotFound), nil
	}
	if err != nil {
		return MaterialResult{}, fmt.Errorf("карточка заявки: %w", err)
	}
	if card.Request.AuthorID != userID || card.Request.Status == request.StatusDeleted {
		return m.result(MatNotFound), nil
	}
	if offset < 0 {
		offset = 0
	}
	total, err := m.Comments.Count(ctx, requestID)
	if err != nil {
		return MaterialResult{}, fmt.Errorf("число материалов: %w", err)
	}
	list, err := m.Comments.List(ctx, requestID, offset, m.cfg.PageSize)
	if err != nil {
		return MaterialResult{}, fmt.Errorf("материалы заявки: %w", err)
	}
	res := m.result(MatMaterials)
	res.Card, res.Comments, res.Offset, res.Total = card, list, offset, total
	return res, nil
}

func (m *Materials) startInput(ctx context.Context, requestID, userID int64, state State, kind MaterialKind) (MaterialResult, error) {
	res, err := m.View(ctx, requestID, userID)
	if err != nil {
		return MaterialResult{}, err
	}
	switch res.Kind {
	case MatAuthorCard:
		return m.result(MatOwnRequest), nil
	case MatNeighborCard:
	default:
		return res, nil
	}
	if !res.CanContribute {
		res.Kind = MatNotActive
		return res, nil
	}
	if err := saveDraft(ctx, m.Store, m.cfg.FSMTTL, userID, Draft{State: state, RequestID: requestID}); err != nil {
		return MaterialResult{}, err
	}
	res.Kind = kind
	return res, nil
}

func (m *Materials) result(kind MaterialKind) MaterialResult {
	return MaterialResult{Kind: kind, Limits: m.cfg.Policy}
}
