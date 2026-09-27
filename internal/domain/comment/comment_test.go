package comment

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/domain/request"
)

func TestCheckCanAdd(t *testing.T) {
	tests := []struct {
		status request.Status
		user   int64
		want   error
	}{
		{request.StatusOpen, 2, nil},
		{request.StatusInProgress, 2, nil},
		{request.StatusOpen, 1, ErrOwnRequest},
		{request.StatusClosed, 2, request.ErrNotActive},
		{request.StatusExpired, 2, request.ErrNotActive},
		{request.StatusDeleted, 2, request.ErrNotActive},
	}
	for _, tt := range tests {
		r := request.Request{AuthorID: 1, Status: tt.status}
		assert.Equal(t, tt.want, CheckCanAdd(r, tt.user), "%s user=%d", tt.status, tt.user)
	}
}

func TestValidate(t *testing.T) {
	p := request.CreatePolicy{MaxBodyLen: 5, MaxAttachments: 2}
	tests := []struct {
		name string
		kind Kind
		body string
		n    int
		want error
	}{
		{"комментарий", KindComment, "текст", 0, nil},
		{"комментарий с вложением", KindComment, "текст", 1, nil},
		{"пустой комментарий", KindComment, "", 1, request.ErrEmptyBody},
		{"длинный комментарий", KindComment, "шесть!", 0, request.ErrBodyTooLong},
		{"доказательство без подписи", KindEvidence, "", 1, nil},
		{"доказательство с подписью", KindEvidence, "фото", 2, nil},
		{"доказательство без медиа", KindEvidence, "текст", 0, ErrNoMedia},
		{"длинная подпись", KindEvidence, "шесть!", 1, request.ErrBodyTooLong},
		{"много вложений", KindEvidence, "", 3, request.ErrTooManyMedia},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorIs(t, Validate(tt.kind, tt.body, tt.n, p), tt.want)
			if tt.want == nil {
				assert.NoError(t, Validate(tt.kind, tt.body, tt.n, p))
			}
		})
	}
}
