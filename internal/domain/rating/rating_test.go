package rating

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/requesttype"
)

func defaultPolicy() Policy {
	return Policy{MinVotes: 1, ConfirmShare: 0.5, RefuteShare: 0.5, DeltaConfirmed: 1, DeltaRefuted: 1, AppliesTo: All()}
}

func TestDelta_Branches(t *testing.T) {
	tests := []struct {
		name      string
		policy    func(p *Policy)
		code      requesttype.Code
		confirms  int
		refutes   int
		wantDelta int
		wantWhy   Reason
		wantOK    bool
	}{
		{name: "тип не входит в список", policy: func(p *Policy) {
			p.AppliesTo, _ = ParseAppliesTo("complaint")
		}, code: requesttype.Poll, confirms: 5},
		{name: "тип входит в список", policy: func(p *Policy) {
			p.AppliesTo, _ = ParseAppliesTo("complaint, poll")
		}, code: requesttype.Poll, confirms: 5, wantDelta: 1, wantWhy: ReasonConfirmed, wantOK: true},
		{name: "нет голосов", code: requesttype.Complaint},
		{name: "голосов меньше минимума", policy: func(p *Policy) { p.MinVotes = 3 },
			code: requesttype.Complaint, confirms: 2},
		{name: "голосов ровно минимум", policy: func(p *Policy) { p.MinVotes = 3 },
			code: requesttype.Complaint, confirms: 3, wantDelta: 1, wantWhy: ReasonConfirmed, wantOK: true},
		{name: "большинство За", code: requesttype.Complaint, confirms: 2, refutes: 1,
			wantDelta: 1, wantWhy: ReasonConfirmed, wantOK: true},
		{name: "большинство Против", code: requesttype.Complaint, confirms: 1, refutes: 2,
			wantDelta: -1, wantWhy: ReasonRefuted, wantOK: true},
		{name: "ничья — нейтрально", code: requesttype.Complaint, confirms: 2, refutes: 2},
		{name: "доля За ровно на пороге — нет", policy: func(p *Policy) { p.ConfirmShare = 0.6; p.RefuteShare = 0.9 },
			code: requesttype.Complaint, confirms: 3, refutes: 2},
		{name: "свои величины дельт", policy: func(p *Policy) { p.DeltaRefuted = 3 },
			code: requesttype.Complaint, refutes: 1, wantDelta: -3, wantWhy: ReasonRefuted, wantOK: true},
		{name: "нулевая дельта — без события", policy: func(p *Policy) { p.DeltaConfirmed = 0 },
			code: requesttype.Complaint, confirms: 1},
		{name: "новый тип при all", code: "new_type", confirms: 1, wantDelta: 1, wantWhy: ReasonConfirmed, wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := defaultPolicy()
			if tt.policy != nil {
				tt.policy(&p)
			}
			delta, why, ok := p.Delta(tt.code, tt.confirms, tt.refutes)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantDelta, delta)
			assert.Equal(t, tt.wantWhy, why)
		})
	}
}

func TestParseAppliesTo(t *testing.T) {
	a, err := ParseAppliesTo("ALL")
	require.NoError(t, err)
	assert.True(t, a.Includes("anything"))

	a, err = ParseAppliesTo(" complaint ,initiative")
	require.NoError(t, err)
	assert.True(t, a.Includes(requesttype.Complaint))
	assert.True(t, a.Includes(requesttype.Initiative))
	assert.False(t, a.Includes(requesttype.Poll))

	for _, bad := range []string{"", "  ", "complaint,,poll", ","} {
		_, err := ParseAppliesTo(bad)
		assert.Error(t, err, bad)
	}
}
