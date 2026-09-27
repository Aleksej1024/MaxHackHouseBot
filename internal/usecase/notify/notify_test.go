package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"maxhouse/internal/domain/comment"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/usecase"
)

type stubCards struct{ card usecase.RequestCard }

func (s stubCards) LoadCard(context.Context, int64) (usecase.RequestCard, error) { return s.card, nil }

type stubSettings struct {
	m          membership.Membership
	err        error
	recipients []int64
	except     int64
}

func (s *stubSettings) GetMembership(context.Context, int64, int64) (membership.Membership, error) {
	return s.m, s.err
}
func (s *stubSettings) SetForwardToDM(context.Context, int64, int64, bool) error     { return nil }
func (s *stubSettings) SetNotifyMaterials(context.Context, int64, int64, bool) error { return nil }
func (s *stubSettings) ForwardRecipients(_ context.Context, _, except int64) ([]int64, error) {
	s.except = except
	return s.recipients, nil
}

type fakeDM struct{ marked []int64 }

func (f *fakeDM) MarkDMUnavailable(_ context.Context, uid int64) error {
	f.marked = append(f.marked, uid)
	return nil
}

// mockOut — ошибки по получателю задаются в errs.
type mockOut struct {
	forwarded, notified []int64
	errs                map[int64]error
}

func (m *mockOut) SendBroadcast(context.Context, int64, request.Request, string) error { return nil }
func (m *mockOut) NotifyMaterial(_ context.Context, uid int64, _ request.Request, _ comment.Comment) error {
	m.notified = append(m.notified, uid)
	return m.errs[uid]
}
func (m *mockOut) ForwardRequest(_ context.Context, uid int64, _ usecase.RequestCard) error {
	m.forwarded = append(m.forwarded, uid)
	return m.errs[uid]
}

func newNotifier(set *stubSettings, dm *fakeDM, out *mockOut) *Notifier {
	card := usecase.RequestCard{Request: request.Request{ID: 5, ChatID: 100, AuthorID: 1}}
	return New(stubCards{card: card}, set, dm, out, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestForwardNew(t *testing.T) {
	set := &stubSettings{recipients: []int64{2, 3, 4}}
	dm, out := &fakeDM{}, &mockOut{errs: map[int64]error{
		3: usecase.ErrDMUnavailable,
		4: errors.New("timeout"),
	}}

	d := newNotifier(set, dm, out).ForwardNew(context.Background(), 5)

	assert.Equal(t, int64(1), set.except, "автору заявка не пересылается")
	assert.Equal(t, []int64{2, 3, 4}, out.forwarded)
	assert.Equal(t, Delivery{Delivered: 1, Unavailable: 1, Failed: 1}, d)
	assert.Equal(t, []int64{3}, dm.marked, "без личного диалога — помечаем, не ошибка")
}

func TestMaterialAdded(t *testing.T) {
	req := request.Request{ID: 5, ChatID: 100, AuthorID: 1}
	c := comment.Comment{Kind: comment.KindComment}

	t.Run("уведомления включены, новые заявки в личку — нет", func(t *testing.T) {
		out := &mockOut{}
		n := newNotifier(&stubSettings{m: membership.Membership{NotifyMaterials: true}}, &fakeDM{}, out)
		assert.True(t, n.MaterialAdded(context.Background(), req, c))
		assert.Equal(t, []int64{1}, out.notified)
	})

	t.Run("уведомления выключены, новые заявки в личку — да", func(t *testing.T) {
		out := &mockOut{}
		n := newNotifier(&stubSettings{m: membership.Membership{ForwardToDM: true}}, &fakeDM{}, out)
		assert.False(t, n.MaterialAdded(context.Background(), req, c))
		assert.Empty(t, out.notified, "материал остаётся в карточке без уведомления")
	})

	t.Run("автор вышел из чата", func(t *testing.T) {
		out := &mockOut{}
		n := newNotifier(&stubSettings{err: usecase.ErrNotFound}, &fakeDM{}, out)
		assert.False(t, n.MaterialAdded(context.Background(), req, c))
		assert.Empty(t, out.notified)
	})

	t.Run("личка недоступна", func(t *testing.T) {
		dm, out := &fakeDM{}, &mockOut{errs: map[int64]error{1: usecase.ErrDMUnavailable}}
		n := newNotifier(&stubSettings{m: membership.Membership{NotifyMaterials: true}}, dm, out)
		assert.False(t, n.MaterialAdded(context.Background(), req, c))
		assert.Equal(t, []int64{1}, dm.marked)
	})
}

func TestDeliver_StopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	n := newNotifier(&stubSettings{}, &fakeDM{}, &mockOut{})
	d := n.Deliver(ctx, []int64{1, 2, 3}, func(context.Context, int64) error {
		calls++
		cancel()
		return nil
	})
	assert.Equal(t, 1, calls)
	assert.Equal(t, Delivery{Delivered: 1, Failed: 2}, d)
}
