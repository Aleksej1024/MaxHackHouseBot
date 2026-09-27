// Package webhook — приём обновлений MAX в режиме UPDATES_MODE=webhook
// обработчик /webhook и подписка при старте.
// HTTP-сервер общий для обоих режимов — пакет httpserver.
package webhook

import (
	"context"
	"net/http"

	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

// Acceptor принимает апдейт без ожидания (intake.Intake).
type Acceptor interface {
	Accept(ctx context.Context, upd model.Update)
}

// Handler — обработчик POST /webhook. Библиотека MAX проверяет секрет
// (401 при несовпадении) и метод; апдейт уходит в intake синхронно,
// поэтому ответ 200 OK возвращается быстро, обработка — после ответа.
func Handler(api *maxbot.Api, secret string, in Acceptor) http.Handler {
	return api.GetHandler(in.Accept, secret)
}
