package maxbot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	maxbotapi "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"

	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/problem"
)

// SendProblemAlert реализует usecase.ProblemAlerter: оповещение о массовой
// проблеме в домовой чат с упоминанием админов. Упоминание в MAX —
// markdown-ссылка max://user/<id>, поэтому сообщение уходит в формате markdown.
func (m *Messenger) SendProblemAlert(ctx context.Context, chatID int64, code problem.Code, admins []membership.User) error {
	text := problemAlertText(code, admins)
	return m.rt.do(ctx, func(ctx context.Context) error {
		msg := maxbotapi.NewMessage().SetChat(chatID).SetText(text).SetFormat(model.FormatMarkdown)
		_, err := m.api.Messages.Send(ctx, msg)
		return err
	})
}

func problemAlertText(code problem.Code, admins []membership.User) string {
	text := fmt.Sprintf(ProblemAlert, markdownPlain(ProblemNames[code]))
	if len(admins) == 0 {
		return text
	}
	mentions := make([]string, 0, len(admins))
	for _, a := range admins {
		name := markdownPlain(a.Nickname)
		if name == "" {
			name = ProblemAdminUnnamed
		}
		mentions = append(mentions, "["+name+"](max://user/"+strconv.FormatInt(a.ID, 10)+")")
	}
	return text + fmt.Sprintf(ProblemAlertAdmins, strings.Join(mentions, ", "))
}

// markdownPlain убирает из ника символы разметки MAX, чтобы ник не сломал
// ссылку-упоминание и не превратился в форматирование.
func markdownPlain(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '[', ']', '(', ')', '*', '_', '~', '+', '^', '`', '\\', '#', '>':
			return -1
		case '\n', '\r':
			return ' '
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}
