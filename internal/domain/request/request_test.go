package request

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckCanCreate(t *testing.T) {
	p := CreatePolicy{DailyLimit: 3, MinRating: -3}
	tests := []struct {
		name    string
		rating  int
		created int
		want    error
	}{
		{name: "всё в норме", rating: 0, created: 0, want: nil},
		{name: "на единицу ниже лимита", rating: 0, created: 2, want: nil},
		{name: "ровно на лимите", rating: 0, created: 3, want: ErrDailyLimit},
		{name: "выше лимита", rating: 0, created: 5, want: ErrDailyLimit},
		{name: "рейтинг ровно на пороге", rating: -3, created: 0, want: nil},
		{name: "рейтинг ниже порога", rating: -4, created: 0, want: ErrRatingTooLow},
		{name: "рейтинг проверяется первым", rating: -4, created: 3, want: ErrRatingTooLow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, p.CheckCanCreate(tt.rating, tt.created))
		})
	}
}

func TestCheckRating(t *testing.T) {
	p := CreatePolicy{DailyLimit: 0, MinRating: -3}
	require.NoError(t, p.CheckRating(-3), "ровно на пороге")
	require.ErrorIs(t, p.CheckRating(-4), ErrRatingTooLow)
	require.NoError(t, p.CheckRating(0), "лимит на число заявок здесь не проверяется")
}

func TestCheckBody(t *testing.T) {
	p := CreatePolicy{MaxBodyLen: 5}
	require.ErrorIs(t, p.CheckBody(""), ErrEmptyBody)
	require.NoError(t, p.CheckBody("пять!"), "длина в символах, а не в байтах")
	require.ErrorIs(t, p.CheckBody("шесть!"), ErrBodyTooLong)
	assert.NoError(t, CreatePolicy{}.CheckBody(strings.Repeat("a", 10000)), "0 — без ограничения")
}

func TestCheckAttachments(t *testing.T) {
	p := CreatePolicy{MaxAttachments: 2}
	require.NoError(t, p.CheckAttachments(2))
	assert.ErrorIs(t, p.CheckAttachments(3), ErrTooManyMedia)
}

func TestDeadlines(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	voting, expires := Deadlines(now, 72*time.Hour, 14*24*time.Hour)
	assert.Equal(t, now.Add(72*time.Hour), voting)
	assert.Equal(t, now.Add(14*24*time.Hour), expires)
}

func TestVotingOpen(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	open := Request{Status: StatusOpen, VotingEndsAt: now.Add(time.Minute)}
	assert.True(t, open.VotingOpen(now))
	assert.False(t, open.VotingOpen(now.Add(time.Minute)), "ровно в момент окончания голосование закрыто")

	for _, st := range []Status{StatusInProgress, StatusClosed, StatusExpired, StatusDeleted} {
		r := open
		r.Status = st
		assert.False(t, r.VotingOpen(now), st)
	}
}

func TestTransitions(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	all := []Status{StatusOpen, StatusInProgress, StatusClosed, StatusExpired, StatusDeleted}
	active := map[Status]bool{StatusOpen: true, StatusInProgress: true}

	for _, st := range all {
		t.Run(string(st), func(t *testing.T) {
			r := Request{Status: st}
			assert.Equal(t, active[st], r.IsActive())

			c := r
			ended, err := c.Close("итог", now)
			if active[st] {
				require.NoError(t, err)
				assert.Equal(t, StatusClosed, c.Status)
				assert.Equal(t, "итог", c.ResultText)
				assert.Equal(t, now, c.ClosedAt)
				assert.Equal(t, st == StatusOpen, ended, "закрытие из open завершает голосование")
			} else {
				require.ErrorIs(t, err, ErrNotActive)
				assert.Equal(t, st, c.Status)
			}

			e := r
			ended, err = e.Expire(now)
			if active[st] {
				require.NoError(t, err)
				assert.Equal(t, StatusExpired, e.Status)
				assert.Equal(t, st == StatusOpen, ended)
			} else {
				require.ErrorIs(t, err, ErrNotActive)
			}

			d := r
			if active[st] {
				require.NoError(t, d.Delete())
				assert.Equal(t, StatusDeleted, d.Status)
			} else {
				require.ErrorIs(t, d.Delete(), ErrNotActive)
			}

			v := r
			if st == StatusOpen {
				require.NoError(t, v.EndVoting())
				assert.Equal(t, StatusInProgress, v.Status)
			} else {
				assert.ErrorIs(t, v.EndVoting(), ErrNotActive)
			}
		})
	}
}
