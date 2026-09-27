package maxbot

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/problem"
	"maxhouse/internal/usecase/problems"
)

// stubProblems возвращает заготовленный результат и запоминает код.
type stubProblems struct {
	res   problems.Result
	err   error
	calls []string
	code  problem.Code
}

func (s *stubProblems) List(context.Context, int64) (problems.Result, error) {
	s.calls = append(s.calls, "List")
	return s.res, s.err
}

func (s *stubProblems) Report(_ context.Context, _ int64, code problem.Code) (problems.Result, error) {
	s.calls = append(s.calls, "Report")
	s.code = code
	return s.res, s.err
}

func TestRouter_ProblemList(t *testing.T) {
	f := newRouterFixture()
	f.prob.res = problems.Result{Outcome: problems.OutcomeList, Problems: problem.Codes}

	f.r.Process(context.Background(), dmCallback(callbackData(cbMenu, menuProblem)))

	require.Len(t, f.out.dm, 1)
	assert.Equal(t, ProblemChoose, f.out.dm[0].text)
	labels, payloads := buttons(f.out.dm[0].kb)
	want := make([][]string, 0, len(problem.Codes)+1)
	for _, c := range problem.Codes {
		want = append(want, []string{ProblemNames[c]})
	}
	want = append(want, []string{BtnToMenu})
	assert.Equal(t, want, labels)
	assert.Equal(t, "pr:no_internet", payloads[0], "в payload только код проблемы")
}

func TestRouter_ProblemReport(t *testing.T) {
	name := ProblemNames[problem.NoWater]
	tests := []struct {
		name       string
		res        problems.Result
		err        error
		wantAnswer string
		wantText   string
	}{
		{
			name:       "учтено",
			res:        problems.Result{Outcome: problems.OutcomeRecorded, Problem: problem.NoWater},
			wantAnswer: NotifyOK,
			wantText:   fmt.Sprintf(ProblemRecorded, name),
		},
		{
			name:       "учтено и отправлено оповещение",
			res:        problems.Result{Outcome: problems.OutcomeRecorded, Problem: problem.NoWater, Alerted: true},
			wantAnswer: NotifyOK,
			wantText:   fmt.Sprintf(ProblemRecorded, name) + ProblemAlerted,
		},
		{
			name:       "уже сообщал",
			res:        problems.Result{Outcome: problems.OutcomeAlreadyReported, Problem: problem.NoWater},
			wantAnswer: NotifyOK,
			wantText:   fmt.Sprintf(ProblemAlready, name),
		},
		{
			name:       "неизвестный код",
			res:        problems.Result{Outcome: problems.OutcomeUnknown},
			wantAnswer: NotifyUnavailable,
		},
		{
			name:       "ошибка",
			err:        errors.New("pg down"),
			wantAnswer: NotifyError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRouterFixture()
			f.prob.res, f.prob.err = tt.res, tt.err

			f.r.Process(context.Background(), dmCallback("pr:no_water"))

			assert.Equal(t, problem.NoWater, f.prob.code)
			require.Len(t, f.out.answers, 1)
			assert.Equal(t, tt.wantAnswer, f.out.answers[0].notification)
			if tt.wantText == "" {
				assert.Empty(t, f.out.dm)
				return
			}
			require.Len(t, f.out.dm, 1)
			assert.Equal(t, tt.wantText, f.out.dm[0].text)
			labels, _ := buttons(f.out.dm[0].kb)
			assert.Equal(t, [][]string{{BtnToMenu}}, labels)
		})
	}
}

func TestRouter_ProblemNotInHouseChat(t *testing.T) {
	f := newRouterFixture()
	f.r.Process(context.Background(), chatCallback("pr:no_water"))
	assert.Empty(t, f.prob.calls, "в домовом чате кнопка не работает")
	require.Len(t, f.out.answers, 1)
	assert.Equal(t, NotifyUnavailable, f.out.answers[0].notification)
}
