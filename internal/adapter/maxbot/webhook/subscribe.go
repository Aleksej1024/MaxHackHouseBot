package webhook

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

// Subscriptions — методы подписок MAX API (maxbot.Api.Subscriptions).
type Subscriptions interface {
	GetSubscriptions(ctx context.Context) (model.GetSubscriptionsResult, error)
	Subscribe(ctx context.Context, url, secret string, updateTypes []string, version string) (model.SimpleQueryResult, error)
	Unsubscribe(ctx context.Context, url string) (model.SimpleQueryResult, error)
}

// UpdateTypes — обновления, которые обрабатывает бот.
var UpdateTypes = []string{
	string(model.UpdateMessageCreated),
	string(model.UpdateMessageCallback),
	string(model.UpdateBotAdded),
	string(model.UpdateBotRemoved),
	string(model.UpdateUserAdded),
	string(model.UpdateUserRemoved),
	string(model.UpdateBotStarted),
	string(model.UpdateChatTitleChanged),
}

// EnsureSubscription подписывает бота на обновления при старте в режиме
// webhook (раздел 11): подписки на другие адреса снимаются, подписка на url
// создаётся заново — так применяются текущие секрет и список типов.
// Без подписки бот не получит ни одного обновления, поэтому ошибка фатальна.
func EnsureSubscription(ctx context.Context, subs Subscriptions, url, secret string, log *slog.Logger) error {
	current, err := subs.GetSubscriptions(ctx)
	if err != nil {
		return fmt.Errorf("список webhook-подписок: %w", err)
	}
	for _, s := range current.Subscriptions {
		if s.URL == url {
			continue
		}
		if _, err := subs.Unsubscribe(ctx, s.URL); err != nil {
			return fmt.Errorf("снятие старой webhook-подписки: %w", err)
		}
		log.Warn("снята webhook-подписка на другой адрес")
	}
	res, err := subs.Subscribe(ctx, url, secret, UpdateTypes, "")
	if err != nil {
		return fmt.Errorf("webhook-подписка: %w", err)
	}
	if !res.Success {
		return fmt.Errorf("webhook-подписка отклонена MAX: %s", res.Message)
	}
	log.Info("webhook-подписка создана", "types", len(UpdateTypes))
	return nil
}
