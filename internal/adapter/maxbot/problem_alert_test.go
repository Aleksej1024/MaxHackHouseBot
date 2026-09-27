package maxbot

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/problem"
)

func TestProblemNames_EveryCode(t *testing.T) {
	for _, c := range problem.Codes {
		assert.NotEmpty(t, ProblemNames[c], "нет названия у проблемы %s", c)
	}
}

func TestProblemAlertText(t *testing.T) {
	tests := []struct {
		name   string
		admins []membership.User
		want   string
	}{
		{
			name: "без админов",
			want: "Уважаемые жители!\n\nФиксируется массовая проблема «Пропала вода».",
		},
		{
			name:   "упоминания админов",
			admins: []membership.User{{ID: 1, Nickname: "Алиса"}, {ID: 2, Nickname: "[Боб](evil) *x*"}, {ID: 3}},
			want: "Уважаемые жители!\n\nФиксируется массовая проблема «Пропала вода»." +
				"\n\nАдминистраторы чата: [Алиса](max://user/1), [Бобevil x](max://user/2), [администратор](max://user/3)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, problemAlertText(problem.NoWater, tt.admins))
		})
	}
}
