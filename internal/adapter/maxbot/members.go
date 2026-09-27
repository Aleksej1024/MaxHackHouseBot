package maxbot

import (
	"context"

	maxbotapi "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"

	"maxhouse/internal/domain/membership"
)

// membersPageSize — размер страницы при запросе участников чата.
const membersPageSize = 100

// MemberLister реализует usecase.MemberLister через MAX API с повторами.
type MemberLister struct {
	api *maxbotapi.Api
	rt  *retrier
}

func NewMemberLister(api *maxbotapi.Api, opts RetryOptions) *MemberLister {
	return &MemberLister{api: api, rt: newRetrier(opts)}
}

// ListMembers возвращает полный список участников чата,
// постранично запрашивая MAX по marker (пока Marker != 0).
func (l *MemberLister) ListMembers(ctx context.Context, chatID int64) ([]membership.Member, error) {
	var out []membership.Member
	var marker int64
	for {
		var page model.ChatMembersList
		err := l.rt.do(ctx, func(ctx context.Context) error {
			res, err := l.api.Chats.GetMembers(ctx, chatID, marker, membersPageSize, nil)
			if err != nil {
				return err
			}
			page = res
			return nil
		})
		if err != nil {
			return nil, err
		}
		for _, m := range page.Members {
			out = append(out, membership.Member{
				UserID:   m.UserID,
				Nickname: nickname(m.Name, m.Username, m.FirstName),
				IsAdmin:  m.IsAdmin || m.IsOwner,
				IsBot:    m.IsBot,
			})
		}
		if page.Marker == 0 {
			break
		}
		marker = page.Marker
	}
	return out, nil
}

// nickname выбирает отображаемый ник пользователя MAX:
// предпочитаем Name, затем Username, затем FirstName (раздел 3, п. 8).
func nickname(name, username, firstName string) string {
	switch {
	case name != "":
		return name
	case username != "":
		return username
	default:
		return firstName
	}
}

// ChatTitle возвращает название чата из MAX (GET /chats/{id}).
func (l *MemberLister) ChatTitle(ctx context.Context, chatID int64) (string, error) {
	var title string
	err := l.rt.do(ctx, func(ctx context.Context) error {
		chat, err := l.api.Chats.GetChat(ctx, chatID)
		if err != nil {
			return err
		}
		title = chat.Title
		return nil
	})
	return title, err
}
