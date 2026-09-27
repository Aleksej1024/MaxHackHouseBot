package chatsync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"maxhouse/internal/domain/request"
	"maxhouse/internal/usecase"
)

type stubCards struct {
	card usecase.RequestCard
	err  error
}

func (s stubCards) LoadCard(context.Context, int64) (usecase.RequestCard, error) {
	return s.card, s.err
}

type mockPublisher struct {
	published, edited, deleted, results int
	err                                 error
}

func (m *mockPublisher) PublishRequest(context.Context, usecase.RequestCard) (string, error) {
	m.published++
	return "mid.new", m.err
}

func (m *mockPublisher) EditRequest(context.Context, usecase.RequestCard) error {
	m.edited++
	return m.err
}

func (m *mockPublisher) DeleteRequest(context.Context, string) error {
	m.deleted++
	return m.err
}

func (m *mockPublisher) PublishResult(context.Context, usecase.RequestCard) error {
	m.results++
	return m.err
}

type fakeState struct {
	mid    string
	failed bool
}

func (f *fakeState) SetChatMessage(_ context.Context, _ int64, mid string) error {
	f.mid, f.failed = mid, false
	return nil
}

func (f *fakeState) SetChatSyncFailed(context.Context, int64) error {
	f.failed = true
	return nil
}

func newSyncer(card usecase.RequestCard, pub *mockPublisher, st *fakeState) *Syncer {
	return New(stubCards{card: card}, pub, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestSync_EditsExistingMessage(t *testing.T) {
	pub, st := &mockPublisher{}, &fakeState{failed: true}
	ok := newSyncer(usecase.RequestCard{Request: request.Request{ID: 1, ChatMessageID: "mid.1"}}, pub, st).Sync(context.Background(), 1)

	assert.True(t, ok)
	assert.Equal(t, 1, pub.edited)
	assert.Zero(t, pub.published)
	assert.False(t, st.failed, "успешная правка снимает chat_sync = failed")
}

func TestSync_RepublishesWhenNoMessage(t *testing.T) {
	pub, st := &mockPublisher{}, &fakeState{failed: true}
	ok := newSyncer(usecase.RequestCard{Request: request.Request{ID: 1}}, pub, st).Sync(context.Background(), 1)

	assert.True(t, ok)
	assert.Equal(t, 1, pub.published)
	assert.Equal(t, "mid.new", st.mid)
	assert.False(t, st.failed)
}

func TestSync_MaxErrorMarksFailed(t *testing.T) {
	pub, st := &mockPublisher{err: errors.New("max down")}, &fakeState{}
	ok := newSyncer(usecase.RequestCard{Request: request.Request{ID: 1, ChatMessageID: "mid.1"}}, pub, st).Sync(context.Background(), 1)

	assert.False(t, ok)
	assert.True(t, st.failed)
}

func TestSync_LoadError(t *testing.T) {
	pub, st := &mockPublisher{}, &fakeState{}
	s := New(stubCards{err: errors.New("pg down")}, pub, st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	assert.False(t, s.Sync(context.Background(), 1))
	assert.Zero(t, pub.edited+pub.published)
}

func TestSync_DeletedRequestRemovesMessage(t *testing.T) {
	pub, st := &mockPublisher{}, &fakeState{}
	card := usecase.RequestCard{Request: request.Request{ID: 1, Status: request.StatusDeleted, ChatMessageID: "mid.1"}}
	ok := newSyncer(card, pub, st).Sync(context.Background(), 1)

	assert.True(t, ok)
	assert.Equal(t, 1, pub.deleted)
	assert.Zero(t, pub.edited+pub.published, "удалённая заявка не публикуется заново")

	pub = &mockPublisher{}
	card.Request.ChatMessageID = ""
	assert.True(t, newSyncer(card, pub, st).Sync(context.Background(), 1))
	assert.Zero(t, pub.deleted+pub.published, "нечего удалять")

	pub = &mockPublisher{err: errors.New("max down")}
	card.Request.ChatMessageID = "mid.1"
	assert.False(t, newSyncer(card, pub, st).Sync(context.Background(), 1))
	assert.True(t, st.failed)
}

func TestPublishResult(t *testing.T) {
	pub, st := &mockPublisher{}, &fakeState{}
	closed := usecase.RequestCard{Request: request.Request{ID: 1, Status: request.StatusClosed, ChatMessageID: "mid.1"}}
	assert.True(t, newSyncer(closed, pub, st).PublishResult(context.Background(), 1))
	assert.Equal(t, 1, pub.results)

	pub = &mockPublisher{}
	open := usecase.RequestCard{Request: request.Request{ID: 1, Status: request.StatusOpen}}
	assert.False(t, newSyncer(open, pub, st).PublishResult(context.Background(), 1))
	assert.Zero(t, pub.results, "итог только у закрытой заявки")

	pub = &mockPublisher{err: errors.New("max down")}
	assert.False(t, newSyncer(closed, pub, &fakeState{}).PublishResult(context.Background(), 1))
}
