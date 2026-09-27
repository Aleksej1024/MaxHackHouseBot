package requests

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"maxhouse/internal/domain/broadcast"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/usecase"
	"maxhouse/internal/usecase/notify"
)

// BroadcastKind — результат шага рассылки (раздел 8.5).
type BroadcastKind int

const (
	BcChooseAudience BroadcastKind = iota + 1
	BcAskText
	BcConfirm
	BcSent
	// BcCooldown — рассылка по заявке была недавно, NextAt — когда можно снова.
	BcCooldown
	BcNotFound
	BcCancelled
	// BcExpired — диалог рассылки устарел (истёк FSM_TTL).
	BcExpired
)

// BroadcastResult — что показать автору.
type BroadcastResult struct {
	Kind     BroadcastKind
	Request  request.Request
	Audience broadcast.Audience
	Text     string
	// Recipients — сколько получателей в аудитории (BcConfirm, BcSent).
	Recipients int
	Delivery   notify.Delivery
	NextAt     time.Time
	Problem    error
	Limits     request.CreatePolicy
}

// Deliverer доставляет сообщения в личку (notify.Notifier).
type Deliverer interface {
	Deliver(ctx context.Context, userIDs []int64, send func(ctx context.Context, userID int64) error) notify.Delivery
}

// BroadcastDeps — зависимости сценария рассылки.
type BroadcastDeps struct {
	Cards      usecase.CardReader
	Locker     usecase.RequestLocker
	Broadcasts usecase.BroadcastRepo
	Out        usecase.UserNotifier
	Deliver    Deliverer
	Store      usecase.FSMStore
	Tx         usecase.TxManager
	Clock      usecase.Clock
	Log        *slog.Logger
}

// BroadcastConfig — параметры из конфига.
type BroadcastConfig struct {
	Policy   request.CreatePolicy // длина текста
	FSMTTL   time.Duration
	Cooldown time.Duration // BROADCAST_COOLDOWN
}

// Broadcasts — рассылка автором сообщения проголосовавшим по заявке.
type Broadcasts struct {
	BroadcastDeps
	cfg BroadcastConfig
}

func NewBroadcasts(deps BroadcastDeps, cfg BroadcastConfig) *Broadcasts {
	return &Broadcasts{BroadcastDeps: deps, cfg: cfg}
}

// Start — кнопка «Разослать сообщение»: проверка авторства и кулдауна,
// затем выбор аудитории.
func (b *Broadcasts) Start(ctx context.Context, requestID, userID int64) (BroadcastResult, error) {
	req, ok, err := b.ownRequest(ctx, requestID, userID)
	if err != nil || !ok {
		return b.result(BcNotFound), err
	}
	if res, blocked, err := b.cooldown(ctx, req); err != nil || blocked {
		return res, err
	}
	res := b.result(BcChooseAudience)
	res.Request = req
	return res, nil
}

// ChooseAudience — выбор аудитории, дальше ждём текст.
func (b *Broadcasts) ChooseAudience(ctx context.Context, requestID, userID int64, aud broadcast.Audience) (BroadcastResult, error) {
	req, ok, err := b.ownRequest(ctx, requestID, userID)
	if err != nil || !ok {
		return b.result(BcNotFound), err
	}
	d := Draft{State: StateBroadcastText, RequestID: requestID, Audience: string(aud)}
	if err := saveDraft(ctx, b.Store, b.cfg.FSMTTL, userID, d); err != nil {
		return BroadcastResult{}, err
	}
	res := b.result(BcAskText)
	res.Request, res.Audience = req, aud
	return res, nil
}

// HandleMessage принимает текст рассылки и показывает подтверждение.
// handled = false, если диалога рассылки нет.
func (b *Broadcasts) HandleMessage(ctx context.Context, userID int64, text string) (BroadcastResult, bool, error) {
	d, found, err := loadDraft(ctx, b.Store, b.Log, userID)
	if err != nil || !found || !d.State.isBroadcast() {
		return BroadcastResult{}, false, err
	}
	aud, _ := broadcast.ParseAudience(d.Audience)
	req, ok, err := b.ownRequest(ctx, d.RequestID, userID)
	if err != nil {
		return BroadcastResult{}, true, err
	}
	if !ok {
		dropDraft(ctx, b.Store, b.Log, userID)
		return b.result(BcNotFound), true, nil
	}

	var problem error
	if d.State == StateBroadcastText {
		text = strings.TrimSpace(text)
		problem = b.cfg.Policy.CheckBody(text)
		if problem == nil {
			d.Body, d.State = text, StateBroadcastConfirm
		}
	} else {
		problem = ErrUnexpectedInput
	}
	if err := saveDraft(ctx, b.Store, b.cfg.FSMTTL, userID, d); err != nil {
		return BroadcastResult{}, true, err
	}

	res := b.result(BcAskText)
	res.Request, res.Audience, res.Problem = req, aud, problem
	if d.State == StateBroadcastConfirm {
		ids, err := b.Broadcasts.Recipients(ctx, req.ID, aud)
		if err != nil {
			return BroadcastResult{}, true, fmt.Errorf("получатели рассылки: %w", err)
		}
		res.Kind, res.Text, res.Recipients = BcConfirm, d.Body, len(ids)
	}
	return res, true, nil
}

// Send — подтверждённая рассылка. Кулдаун проверяется повторно под
// блокировкой заявки, факт рассылки пишется до отправки: сбой во время
// доставки не позволит обойти кулдаун повтором.
func (b *Broadcasts) Send(ctx context.Context, userID int64) (BroadcastResult, error) {
	d, found, err := loadDraft(ctx, b.Store, b.Log, userID)
	if err != nil {
		return BroadcastResult{}, err
	}
	if !found || d.State != StateBroadcastConfirm {
		return b.result(BcExpired), nil
	}
	aud, _ := broadcast.ParseAudience(d.Audience)

	var res BroadcastResult
	var ids []int64
	err = b.Tx.Do(ctx, func(ctx context.Context) error {
		req, err := b.Locker.GetForUpdate(ctx, d.RequestID)
		if errors.Is(err, usecase.ErrNotFound) || (err == nil && (req.AuthorID != userID || req.Status == request.StatusDeleted)) {
			res = b.result(BcNotFound)
			return nil
		}
		if err != nil {
			return err
		}
		var blocked bool
		if res, blocked, err = b.cooldown(ctx, req); err != nil || blocked {
			return err
		}
		if ids, err = b.Broadcasts.Recipients(ctx, req.ID, aud); err != nil {
			return err
		}
		if err := b.Broadcasts.Record(ctx, req.ID, aud, len(ids), b.Clock.Now()); err != nil {
			return err
		}
		res = b.result(BcSent)
		res.Request, res.Audience, res.Recipients = req, aud, len(ids)
		return nil
	})
	if err != nil {
		return BroadcastResult{}, fmt.Errorf("рассылка по заявке %d: %w", d.RequestID, err)
	}
	dropDraft(ctx, b.Store, b.Log, userID)
	if res.Kind != BcSent {
		return res, nil
	}

	res.Delivery = b.Deliver.Deliver(ctx, ids, func(ctx context.Context, uid int64) error {
		return b.Out.SendBroadcast(ctx, uid, res.Request, d.Body)
	})
	b.Log.Info("рассылка по заявке", "request_id", res.Request.ID, "user_id", userID,
		"audience", string(aud), "recipients", len(ids),
		"delivered", res.Delivery.Delivered, "unavailable", res.Delivery.Unavailable)
	return res, nil
}

// Cancel — отмена рассылки.
func (b *Broadcasts) Cancel(ctx context.Context, userID int64) (BroadcastResult, error) {
	d, found, err := loadDraft(ctx, b.Store, b.Log, userID)
	if err != nil {
		return BroadcastResult{}, err
	}
	if found && d.State.isBroadcast() {
		dropDraft(ctx, b.Store, b.Log, userID)
	}
	return b.result(BcCancelled), nil
}

// ownRequest — заявка существует, не удалена и принадлежит пользователю.
func (b *Broadcasts) ownRequest(ctx context.Context, requestID, userID int64) (request.Request, bool, error) {
	card, err := b.Cards.LoadCard(ctx, requestID)
	if errors.Is(err, usecase.ErrNotFound) {
		return request.Request{}, false, nil
	}
	if err != nil {
		return request.Request{}, false, fmt.Errorf("карточка заявки: %w", err)
	}
	req := card.Request
	if req.AuthorID != userID || req.Status == request.StatusDeleted {
		return request.Request{}, false, nil
	}
	return req, true, nil
}

// cooldown: blocked = true — рассылка сейчас недоступна, res содержит BcCooldown.
func (b *Broadcasts) cooldown(ctx context.Context, req request.Request) (BroadcastResult, bool, error) {
	last, has, err := b.Broadcasts.LastAt(ctx, req.ID)
	if err != nil {
		return BroadcastResult{}, false, fmt.Errorf("последняя рассылка: %w", err)
	}
	switch next, err := broadcast.CheckCooldown(last, has, b.Clock.Now(), b.cfg.Cooldown); {
	case errors.Is(err, broadcast.ErrCooldown):
		res := b.result(BcCooldown)
		res.Request, res.NextAt = req, next
		return res, true, nil
	case err != nil:
		return BroadcastResult{}, false, err
	}
	return BroadcastResult{}, false, nil
}

func (b *Broadcasts) result(kind BroadcastKind) BroadcastResult {
	return BroadcastResult{Kind: kind, Limits: b.cfg.Policy}
}
