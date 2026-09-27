// Package request — заявка жителей и правила её создания.
package request

import (
	"errors"
	"time"
	"unicode/utf8"

	"maxhouse/internal/domain/requesttype"
)

// Status — статус заявки (раздел 8.1).
type Status string

const (
	StatusOpen       Status = "open"
	StatusInProgress Status = "in_progress"
	StatusClosed     Status = "closed"
	StatusExpired    Status = "expired"
	StatusDeleted    Status = "deleted"
)

// AttachmentKind — вид вложения MAX, которое принимает бот.
type AttachmentKind string

const (
	KindImage AttachmentKind = "image"
	KindVideo AttachmentKind = "video"
	KindFile  AttachmentKind = "file"
	KindAudio AttachmentKind = "audio"
)

// Attachment — ссылка на медиа, хранящееся в MAX (раздел 3, п. 1).
// Ref — токен вложения MAX.
type Attachment struct {
	Kind AttachmentKind `json:"kind"`
	Ref  string         `json:"ref"`
}

// Request — заявка. Автор хранится всегда, анонимность только скрывает его
// от жителей (раздел 3, п. 5).
type Request struct {
	ID            int64
	ChatID        int64
	AuthorID      int64
	Type          requesttype.Code
	Status        Status
	IsAnonymous   bool
	Body          string
	ChatMessageID string
	ResultText    string
	CreatedAt     time.Time
	VotingEndsAt  time.Time
	ExpiresAt     time.Time
	ClosedAt      time.Time // нулевое, пока заявка не закрыта и не истекла
}

// IsActive — заявка ещё живёт: её можно править, закрыть или удалить
// (статусы open и in_progress, раздел 8.1).
func (r Request) IsActive() bool {
	return r.Status == StatusOpen || r.Status == StatusInProgress
}

// ErrNotActive — действие недоступно: заявка закрыта, истекла или удалена.
var ErrNotActive = errors.New("заявка уже закрыта, истекла или удалена")

// Close закрывает заявку автором с текстом результата. Из open голосование
// завершается досрочно: votingEnded = true, вызывающий начисляет рейтинг.
func (r *Request) Close(result string, now time.Time) (votingEnded bool, err error) {
	if !r.IsActive() {
		return false, ErrNotActive
	}
	votingEnded = r.Status == StatusOpen
	r.Status = StatusClosed
	r.ResultText = result
	r.ClosedAt = now
	return votingEnded, nil
}

// Expire переводит заявку в expired по истечении общего срока. Если шло
// голосование, оно завершается вместе с переводом (votingEnded = true).
func (r *Request) Expire(now time.Time) (votingEnded bool, err error) {
	if !r.IsActive() {
		return false, ErrNotActive
	}
	votingEnded = r.Status == StatusOpen
	r.Status = StatusExpired
	r.ClosedAt = now
	return votingEnded, nil
}

// EndVoting — автоматический переход open → in_progress по VOTING_DURATION.
func (r *Request) EndVoting() error {
	if r.Status != StatusOpen {
		return ErrNotActive
	}
	r.Status = StatusInProgress
	return nil
}

// Delete — мягкое удаление автором. Рейтинг по удалённой заявке
// не начисляется и не снимается.
func (r *Request) Delete() error {
	if !r.IsActive() {
		return ErrNotActive
	}
	r.Status = StatusDeleted
	return nil
}

// VotingOpen — идёт ли голосование: заявка в статусе open и срок
// голосования не истёк. Воркер переводит заявку в in_progress с задержкой
// до SCHEDULER_INTERVAL, поэтому время проверяется явно.
func (r Request) VotingOpen(now time.Time) bool {
	return r.Status == StatusOpen && now.Before(r.VotingEndsAt)
}

// Ошибки правил создания заявки.
var (
	ErrDailyLimit   = errors.New("достигнут суточный лимит заявок")
	ErrRatingTooLow = errors.New("рейтинг ниже порога создания заявок")
	ErrEmptyBody    = errors.New("пустой текст заявки")
	ErrBodyTooLong  = errors.New("текст заявки слишком длинный")
	ErrTooManyMedia = errors.New("слишком много вложений")
)

// CreatePolicy — ограничения на создание заявок (из конфига).
type CreatePolicy struct {
	DailyLimit     int // DAILY_REQUEST_LIMIT, заявок каждого типа за скользящие 24 часа
	MinRating      int // RATING_MIN_TO_CREATE
	MaxBodyLen     int // REQUEST_BODY_MAX_LEN, в символах
	MaxAttachments int // REQUEST_MAX_ATTACHMENTS
}

// LimitWindow — окно суточного лимита (скользящие 24 часа, раздел 4).
const LimitWindow = 24 * time.Hour

// CheckRating проверяет только порог рейтинга (до выбора типа заявки).
func (p CreatePolicy) CheckRating(rating int) error {
	if rating < p.MinRating {
		return ErrRatingTooLow
	}
	return nil
}

// CheckCanCreate проверяет порог рейтинга и суточный лимит.
// createdInWindow — число заявок автора этого типа в этом чате за LimitWindow:
// лимит действует на каждый тип отдельно.
func (p CreatePolicy) CheckCanCreate(rating, createdInWindow int) error {
	if rating < p.MinRating {
		return ErrRatingTooLow
	}
	if createdInWindow >= p.DailyLimit {
		return ErrDailyLimit
	}
	return nil
}

// CheckBody проверяет текст заявки: не пустой и не длиннее MaxBodyLen символов.
func (p CreatePolicy) CheckBody(body string) error {
	if body == "" {
		return ErrEmptyBody
	}
	if p.MaxBodyLen > 0 && utf8.RuneCountInString(body) > p.MaxBodyLen {
		return ErrBodyTooLong
	}
	return nil
}

// CheckAttachments проверяет, что вложений не больше MaxAttachments.
func (p CreatePolicy) CheckAttachments(n int) error {
	if p.MaxAttachments > 0 && n > p.MaxAttachments {
		return ErrTooManyMedia
	}
	return nil
}

// Deadlines вычисляет окончание голосования и общий срок жизни заявки.
func Deadlines(now time.Time, voting, ttl time.Duration) (votingEndsAt, expiresAt time.Time) {
	return now.Add(voting), now.Add(ttl)
}
