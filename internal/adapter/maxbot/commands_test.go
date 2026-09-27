package maxbot

import (
	"context"
	"errors"
	"testing"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/usecase/problems"
	"maxhouse/internal/usecase/requests"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		text string
		item string
		ok   bool
	}{
		{"/create", menuCreate, true},
		{"  /Create  ", menuCreate, true},
		{"/create@house_bot", menuCreate, true},
		{"/problem вода", menuProblem, true},
		{"/start", menuMain, true},
		{"/menu", menuMain, true},
		{"/help", menuHelp, true},
		{"/unknown", "", false},
		{"/", "", false},
		{"create", "", false},
		{"Сосед курит /create", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		item, ok := parseCommand(tt.text)
		assert.Equal(t, tt.ok, ok, tt.text)
		assert.Equal(t, tt.item, item, tt.text)
	}
}

func TestCommands_Described(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range commands {
		assert.False(t, seen[c.name], "команда %s повторяется", c.name)
		seen[c.name] = true
		if c.name != "start" {
			assert.NotEmpty(t, CommandDescriptions[c.name], "нет описания у /%s", c.name)
		}
	}
	for _, c := range botCommands() {
		assert.NotEqual(t, "start", c.Name, "/start клиент MAX показывает сам")
		assert.Contains(t, helpText(), "/"+c.Name+" — "+c.Description, "команда есть в справке")
	}
}

func TestRouter_CommandInterruptsDialog(t *testing.T) {
	f := newRouterFixture()
	f.dlg.flow = requests.FlowCreate // посреди черновика
	f.create.handled = true

	f.r.Process(context.Background(), dmMessage("/create"))

	assert.Equal(t, 1, f.dlg.resets, "команда прерывает диалог")
	assert.Equal(t, []string{"Begin"}, f.create.calls, "команда не стала текстом черновика")
	assert.Empty(t, f.out.answers, "ответа на нажатие у команды нет")
}

func TestRouter_Commands(t *testing.T) {
	t.Run("problem", func(t *testing.T) {
		f := newRouterFixture()
		f.prob.res = problems.Result{Outcome: problems.OutcomeList}
		f.r.Process(context.Background(), dmMessage("/problem"))
		assert.Equal(t, []string{"List"}, f.prob.calls)
		require.Len(t, f.out.dm, 1)
		assert.Equal(t, ProblemChoose, f.out.dm[0].text)
	})
	t.Run("help", func(t *testing.T) {
		f := newRouterFixture()
		f.r.Process(context.Background(), dmMessage("/help"))
		require.Len(t, f.out.dm, 1)
		assert.Equal(t, helpText(), f.out.dm[0].text)
	})
	t.Run("start — меню", func(t *testing.T) {
		f := newRouterFixture()
		f.r.Process(context.Background(), dmMessage("/start"))
		assert.Len(t, f.onb.started, 1)
		assert.Equal(t, 1, f.dlg.resets)
	})
	t.Run("неизвестная команда уходит в диалог как текст", func(t *testing.T) {
		f := newRouterFixture()
		f.dlg.flow = requests.FlowCreate
		f.create.handled = true
		f.r.Process(context.Background(), dmMessage("/unknown"))
		assert.Equal(t, []string{"HandleMessage"}, f.create.calls)
		assert.Zero(t, f.dlg.resets)
	})
}

type mockCommandSetter struct {
	got model.BotPatchCommands
	err error
}

func (m *mockCommandSetter) PatchCommands(_ context.Context, p model.BotPatchCommands) (model.BotPatchCommands, error) {
	m.got = p
	return p, m.err
}

func TestRegisterCommands(t *testing.T) {
	m := &mockCommandSetter{}
	require.NoError(t, RegisterCommands(context.Background(), m))
	assert.Equal(t, botCommands(), m.got.Commands)
	assert.Equal(t, "create", m.got.Commands[0].Name)

	m.err = errors.New("403")
	assert.Error(t, RegisterCommands(context.Background(), m))
}
