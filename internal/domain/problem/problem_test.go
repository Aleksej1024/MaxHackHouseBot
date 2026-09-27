package problem

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var t0 = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func TestKnown(t *testing.T) {
	for _, c := range Codes {
		assert.True(t, Known(c), c)
	}
	assert.False(t, Known("meteor"))
	assert.False(t, Known(""))
}

func TestIncident_Threshold(t *testing.T) {
	p := Policy{Threshold: 3, Window: 30 * time.Minute}
	i := New(1, NoWater, t0)
	assert.Equal(t, t0, i.FirstReportedAt)
	assert.Zero(t, i.Reporters)

	i.AddReporter()
	i.AddReporter()
	assert.False(t, i.NeedsAlert(p), "двое из трёх")

	i.AddReporter()
	assert.True(t, i.NeedsAlert(p), "ровно на пороге — оповещение")

	i.MarkNotified(t0.Add(time.Minute))
	assert.True(t, i.Notified)
	assert.Equal(t, t0.Add(time.Minute), i.NotifiedAt)
	i.AddReporter()
	assert.False(t, i.NeedsAlert(p), "в пределах окна оповещение одно")

	i.ClearNotified()
	assert.False(t, i.Notified)
	assert.True(t, i.NotifiedAt.IsZero())
	assert.True(t, i.NeedsAlert(p), "после неудачной отправки — снова пора")
}

func TestPolicy_OpenSince(t *testing.T) {
	p := Policy{Threshold: 3, Window: 30 * time.Minute}
	assert.Equal(t, t0.Add(-30*time.Minute), p.OpenSince(t0))
}
