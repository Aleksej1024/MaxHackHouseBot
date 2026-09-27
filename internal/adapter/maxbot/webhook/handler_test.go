package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecret = "test-secret"

type recordingAcceptor struct {
	mu   sync.Mutex
	upds []model.Update
}

func (a *recordingAcceptor) Accept(_ context.Context, upd model.Update) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.upds = append(a.upds, upd)
}

const messageCreatedBody = `{"update_type":"message_created","timestamp":1700000000,"user_id":42,"message_id":"mid.1","message":{}}`

func post(t *testing.T, h http.Handler, secret string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", strings.NewReader(messageCreatedBody))
	if secret != "" {
		req.Header.Set(maxbot.SecretHeader, secret)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

func TestHandler_Secret(t *testing.T) {
	api, err := maxbot.NewApi("test-token")
	require.NoError(t, err)
	acc := &recordingAcceptor{}
	h := Handler(api, testSecret, acc)

	assert.Equal(t, http.StatusUnauthorized, post(t, h, ""))
	assert.Equal(t, http.StatusUnauthorized, post(t, h, "wrong"))
	assert.Empty(t, acc.upds, "без верного секрета апдейт не принимается")

	assert.Equal(t, http.StatusOK, post(t, h, testSecret))
	require.Len(t, acc.upds, 1)
	assert.Equal(t, model.UpdateMessageCreated, acc.upds[0].UpdateType)
}
