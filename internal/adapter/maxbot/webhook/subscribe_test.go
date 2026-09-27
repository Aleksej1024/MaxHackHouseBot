package webhook

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSubscriptions struct {
	current      []model.Subscription
	getErr       error
	subscribeRes model.SimpleQueryResult
	subscribeErr error
	unsubscribed []string
	subscribed   []string
	secret       string
	types        []string
}

func (f *fakeSubscriptions) GetSubscriptions(context.Context) (model.GetSubscriptionsResult, error) {
	return model.GetSubscriptionsResult{Subscriptions: f.current}, f.getErr
}

func (f *fakeSubscriptions) Subscribe(_ context.Context, url, secret string, types []string, _ string) (model.SimpleQueryResult, error) {
	f.subscribed = append(f.subscribed, url)
	f.secret, f.types = secret, types
	return f.subscribeRes, f.subscribeErr
}

func (f *fakeSubscriptions) Unsubscribe(_ context.Context, url string) (model.SimpleQueryResult, error) {
	f.unsubscribed = append(f.unsubscribed, url)
	return model.SimpleQueryResult{Success: true}, nil
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

const hookURL = "https://bot.example/webhook"

func TestEnsureSubscription(t *testing.T) {
	f := &fakeSubscriptions{
		current: []model.Subscription{
			{URL: "https://old.example/webhook"},
			{URL: hookURL},
		},
		subscribeRes: model.SimpleQueryResult{Success: true},
	}

	require.NoError(t, EnsureSubscription(context.Background(), f, hookURL, "s3cret", discard))

	assert.Equal(t, []string{"https://old.example/webhook"}, f.unsubscribed, "чужие адреса сняты, свой — нет")
	assert.Equal(t, []string{hookURL}, f.subscribed, "подписка обновляется при каждом старте")
	assert.Equal(t, "s3cret", f.secret)
	assert.Equal(t, UpdateTypes, f.types)
	assert.Contains(t, UpdateTypes, string(model.UpdateChatTitleChanged))
}

func TestEnsureSubscription_Errors(t *testing.T) {
	ctx := context.Background()

	err := EnsureSubscription(ctx, &fakeSubscriptions{getErr: errors.New("401")}, hookURL, "s", discard)
	require.Error(t, err)

	err = EnsureSubscription(ctx, &fakeSubscriptions{subscribeErr: errors.New("400")}, hookURL, "s", discard)
	require.Error(t, err)

	err = EnsureSubscription(ctx, &fakeSubscriptions{
		subscribeRes: model.SimpleQueryResult{Success: false, Message: "bad url"},
	}, hookURL, "s", discard)
	require.ErrorContains(t, err, "bad url")
}
