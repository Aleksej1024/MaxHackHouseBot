package broadcast

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/vote"
)

func TestParseAudience(t *testing.T) {
	for _, a := range Audiences {
		got, ok := ParseAudience(string(a))
		assert.True(t, ok)
		assert.Equal(t, a, got)
	}
	_, ok := ParseAudience("nobody")
	assert.False(t, ok)
}

func TestVotes(t *testing.T) {
	assert.Equal(t, []vote.Value{vote.Confirm, vote.Refute}, AudienceAll.Votes())
	assert.Equal(t, []vote.Value{vote.Confirm}, AudienceConfirm.Votes())
	assert.Equal(t, []vote.Value{vote.Refute}, AudienceRefute.Votes())
}

func TestCheckCooldown(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	cd := 24 * time.Hour

	_, err := CheckCooldown(time.Time{}, false, now, cd)
	require.NoError(t, err, "первая рассылка")

	next, err := CheckCooldown(now.Add(-23*time.Hour), true, now, cd)
	require.ErrorIs(t, err, ErrCooldown)
	assert.Equal(t, now.Add(time.Hour), next)

	_, err = CheckCooldown(now.Add(-cd), true, now, cd)
	assert.NoError(t, err, "ровно через кулдаун можно")
}
