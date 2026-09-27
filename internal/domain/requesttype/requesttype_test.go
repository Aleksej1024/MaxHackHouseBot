package requesttype

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDefault(t *testing.T) {
	r := Default()
	var codes []Code
	for _, tp := range r.All() {
		codes = append(codes, tp.Code)
	}
	assert.Equal(t, []Code{Complaint, Initiative}, codes)

	_, ok := r.Get(Initiative)
	assert.True(t, ok)
	_, ok = r.Get(Poll)
	assert.False(t, ok, "опрос временно отключён")
	_, ok = r.Get("unknown")
	assert.False(t, ok)
}

func TestNew_SkipsDuplicatesAndCopies(t *testing.T) {
	r := New(Type{Code: "a"}, Type{Code: "a", TTL: time.Hour}, Type{Code: "b"})
	all := r.All()
	assert.Len(t, all, 2)
	assert.Zero(t, all[0].TTL, "первое объявление выигрывает")

	all[0].Code = "changed"
	_, ok := r.Get("a")
	assert.True(t, ok, "All возвращает копию")
}

func TestDurations(t *testing.T) {
	tests := []struct {
		name       string
		tp         Type
		wantVoting time.Duration
		wantTTL    time.Duration
	}{
		{name: "значения конфига", tp: Type{Code: "a"}, wantVoting: 72 * time.Hour, wantTTL: 336 * time.Hour},
		{name: "переопределение голосования", tp: Type{Code: "a", VotingDuration: time.Hour}, wantVoting: time.Hour, wantTTL: 336 * time.Hour},
		{name: "переопределение срока", tp: Type{Code: "a", TTL: 48 * time.Hour}, wantVoting: 72 * time.Hour, wantTTL: 48 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			voting, ttl := tt.tp.Durations(72*time.Hour, 336*time.Hour)
			assert.Equal(t, tt.wantVoting, voting)
			assert.Equal(t, tt.wantTTL, ttl)
		})
	}
}
