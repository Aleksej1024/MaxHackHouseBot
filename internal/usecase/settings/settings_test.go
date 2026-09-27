package settings

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/usecase"
)

type stubChats struct{ ok bool }

func (s stubChats) ActiveChat(context.Context, int64) (housechat.Chat, bool, error) {
	return housechat.Chat{ID: 100, Title: "Дом"}, s.ok, nil
}

type fakeMembers struct {
	m   membership.Membership
	err error
}

func (f *fakeMembers) GetMembership(context.Context, int64, int64) (membership.Membership, error) {
	return f.m, f.err
}

func (f *fakeMembers) SetForwardToDM(_ context.Context, _, _ int64, on bool) error {
	f.m.ForwardToDM = on
	return nil
}

func (f *fakeMembers) SetNotifyMaterials(_ context.Context, _, _ int64, on bool) error {
	f.m.NotifyMaterials = on
	return nil
}

func (f *fakeMembers) ForwardRecipients(context.Context, int64, int64) ([]int64, error) {
	return nil, nil
}

func TestGet_CreationStatus(t *testing.T) {
	tests := []struct {
		rating int
		want   bool
	}{{0, true}, {-3, true}, {-4, false}}
	for _, tt := range tests {
		svc := New(stubChats{ok: true}, &fakeMembers{m: membership.Membership{Rating: tt.rating}}, -3)
		res, err := svc.Get(context.Background(), 7)
		require.NoError(t, err)
		assert.Equal(t, tt.want, res.CanCreate, "рейтинг %d", tt.rating)
	}
}

func TestToggleForward(t *testing.T) {
	members := &fakeMembers{}
	svc := New(stubChats{ok: true}, members, -3)

	res, err := svc.ToggleForward(context.Background(), 7)
	require.NoError(t, err)
	assert.True(t, res.ForwardToDM)
	assert.True(t, members.m.ForwardToDM)

	res, err = svc.ToggleForward(context.Background(), 7)
	require.NoError(t, err)
	assert.False(t, res.ForwardToDM)
}

func TestNoChat(t *testing.T) {
	res, err := New(stubChats{}, &fakeMembers{}, -3).Get(context.Background(), 7)
	require.NoError(t, err)
	assert.True(t, res.NoChat)

	res, err = New(stubChats{ok: true}, &fakeMembers{err: usecase.ErrNotFound}, -3).ToggleForward(context.Background(), 7)
	require.NoError(t, err)
	assert.True(t, res.NoChat)
}

func TestToggleNotifyMaterials(t *testing.T) {
	members := &fakeMembers{m: membership.Membership{NotifyMaterials: true}}
	svc := New(stubChats{ok: true}, members, -3)

	res, err := svc.Get(context.Background(), 7)
	require.NoError(t, err)
	assert.True(t, res.NotifyMaterials)
	assert.False(t, res.ForwardToDM, "настройки независимы")

	res, err = svc.ToggleNotifyMaterials(context.Background(), 7)
	require.NoError(t, err)
	assert.False(t, res.NotifyMaterials)
	assert.False(t, members.m.NotifyMaterials)
	assert.False(t, members.m.ForwardToDM, "вторая настройка не меняется")

	res, err = New(stubChats{}, members, -3).ToggleNotifyMaterials(context.Background(), 7)
	require.NoError(t, err)
	assert.True(t, res.NoChat)
}
