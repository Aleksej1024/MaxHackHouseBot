// Package comment — комментарии и доказательства жителей к заявке
// Их видит только автор заявки, вместе с ником
// отправителя; в чат уходит лишь счётчик.
package comment

import (
	"errors"
	"time"

	"maxhouse/internal/domain/request"
)

// Kind — вид материала.
type Kind string

const (
	KindComment  Kind = "comment"  // текстовый комментарий
	KindEvidence Kind = "evidence" // доказательство: фото, видео или файл
)

// Comment — материал к заявке. Вложения — ссылки на медиа MAX.
type Comment struct {
	ID             int64
	RequestID      int64
	AuthorID       int64
	AuthorNickname string
	Kind           Kind
	Body           string
	Attachments    []request.Attachment
	CreatedAt      time.Time
}

var (
	// ErrOwnRequest — автор не добавляет материалы к своей заявке.
	ErrOwnRequest = errors.New("нельзя добавлять материалы к своей заявке")
	// ErrNoMedia — доказательство без вложений.
	ErrNoMedia = errors.New("доказательство без вложений")
)

// CheckCanAdd — можно ли пользователю добавить материал к заявке:
// заявка не закрыта, не истекла и не удалена, пользователь — не автор.
// Участие в чате проверяется отдельно.
func CheckCanAdd(req request.Request, userID int64) error {
	if !req.IsActive() {
		return request.ErrNotActive
	}
	if req.AuthorID == userID {
		return ErrOwnRequest
	}
	return nil
}

// Validate проверяет содержимое материала. Комментарий — обязательный текст
// (вложения можно), доказательство — хотя бы одно вложение и необязательная
// подпись. Длина текста и число вложений — по политике заявок.
func Validate(kind Kind, body string, attachments int, p request.CreatePolicy) error {
	switch kind {
	case KindEvidence:
		if attachments == 0 {
			return ErrNoMedia
		}
		if body != "" {
			if err := p.CheckBody(body); err != nil {
				return err
			}
		}
	default:
		if err := p.CheckBody(body); err != nil {
			return err
		}
	}
	return p.CheckAttachments(attachments)
}
