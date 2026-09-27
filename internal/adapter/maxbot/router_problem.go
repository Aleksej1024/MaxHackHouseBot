package maxbot

import (
	"context"
	"fmt"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"

	"maxhouse/internal/domain/problem"
	"maxhouse/internal/usecase/problems"
)

// ProblemScenario — «Сообщить о проблеме»
type ProblemScenario interface {
	List(ctx context.Context, userID int64) (problems.Result, error)
	Report(ctx context.Context, userID int64, code problem.Code) (problems.Result, error)
}

// onProblem — выбор проблемы "pr:<код>". Код проверяет use case.
func (r *Router) onProblem(ctx context.Context, cb model.Callback, param string, attrs []any) {
	res, err := r.prob.Report(ctx, cb.User.UserID, problem.Code(param))
	r.answerProblem(ctx, cb, res, err, attrs)
}

func (r *Router) answerProblem(ctx context.Context, cb model.Callback, res problems.Result, err error, attrs []any) {
	if err != nil {
		r.log.Error("сообщение о проблеме", append(attrs, "error", err)...)
		r.answer(ctx, cb.CallbackID, NotifyError, attrs)
		return
	}
	userID := cb.User.UserID
	switch res.Outcome {
	case problems.OutcomeNoChat:
		r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
		r.onDialogStart(ctx, cb.User, attrs)
	case problems.OutcomeList:
		r.answer(ctx, cb.CallbackID, BtnReportProblem, attrs)
		r.send(ctx, userID, ProblemChoose, problemKeyboard(res.Problems), attrs)
	case problems.OutcomeRecorded, problems.OutcomeAlreadyReported:
		tmpl := ProblemRecorded
		if res.Outcome == problems.OutcomeAlreadyReported {
			tmpl = ProblemAlready
		}
		text := fmt.Sprintf(tmpl, ProblemNames[res.Problem])
		if res.Alerted {
			text += ProblemAlerted
		}
		r.answer(ctx, cb.CallbackID, NotifyOK, attrs)
		r.send(ctx, userID, text, toMenuKeyboard(), attrs)
	default:
		r.answer(ctx, cb.CallbackID, NotifyUnavailable, attrs)
	}
}
