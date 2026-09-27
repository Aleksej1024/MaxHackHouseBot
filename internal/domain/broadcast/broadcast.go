// Package broadcast — рассылка автором заявки сообщения проголосовавшим
package broadcast

import (
	"errors"
	"time"

	"maxhouse/internal/domain/vote"
)

// Audience — кому рассылать.
type Audience string

const (
	AudienceAll     Audience = "all"     // все проголосовавшие
	AudienceConfirm Audience = "confirm" // проголосовавшие «За» (в опросе «Да»)
	AudienceRefute  Audience = "refute"  // проголосовавшие «Против» (в опросе «Нет»)
)

// Audiences — все аудитории в порядке показа.
var Audiences = []Audience{AudienceAll, AudienceConfirm, AudienceRefute}

// ParseAudience разбирает код аудитории.
func ParseAudience(s string) (Audience, bool) {
	for _, a := range Audiences {
		if string(a) == s {
			return a, true
		}
	}
	return "", false
}

// Votes возвращает значения голосов, попадающие в аудиторию.
func (a Audience) Votes() []vote.Value {
	switch a {
	case AudienceConfirm:
		return []vote.Value{vote.Confirm}
	case AudienceRefute:
		return []vote.Value{vote.Refute}
	}
	return []vote.Value{vote.Confirm, vote.Refute}
}

// ErrCooldown — рассылка по этой заявке была недавно.
var ErrCooldown = errors.New("рассылка по заявке была недавно")

// CheckCooldown — прошёл ли BROADCAST_COOLDOWN с последней рассылки.
// hasLast = false — рассылок ещё не было. Возвращает время, с которого
// рассылка снова доступна.
func CheckCooldown(last time.Time, hasLast bool, now time.Time, cooldown time.Duration) (time.Time, error) {
	if !hasLast {
		return now, nil
	}
	next := last.Add(cooldown)
	if now.Before(next) {
		return next, ErrCooldown
	}
	return now, nil
}
