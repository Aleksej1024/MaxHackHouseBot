// Package rating — правила рейтинга жителя в домовом чате.
// Рейтинг привязан к паре (пользователь, чат), вес голоса одинаков,
// все пороги и величины изменения — из конфига.
package rating

import (
	"errors"
	"fmt"
	"strings"

	"maxhouse/internal/domain/requesttype"
)

// Reason — причина изменения рейтинга (rating_events.reason).
type Reason string

const (
	ReasonConfirmed Reason = "confirmed" // заявку поддержали
	ReasonRefuted   Reason = "refuted"   // заявку опровергли
	ReasonReset     Reason = "reset"     // периодический сброс отрицательного рейтинга
)

// AppliesTo — типы заявок, влияющие на рейтинг (RATING_APPLIES_TO_TYPES).
type AppliesTo struct {
	all   bool
	codes map[requesttype.Code]bool
}

// All — рейтинг считается для заявок любых типов, включая новые.
func All() AppliesTo { return AppliesTo{all: true} }

// ParseAppliesTo разбирает "all" или список кодов через запятую.
func ParseAppliesTo(s string) (AppliesTo, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return AppliesTo{}, errors.New("пустой список типов заявок")
	}
	if strings.EqualFold(s, "all") {
		return All(), nil
	}
	codes := map[requesttype.Code]bool{}
	for _, part := range strings.Split(s, ",") {
		code := strings.TrimSpace(part)
		if code == "" {
			return AppliesTo{}, fmt.Errorf("пустой код типа в списке %q", s)
		}
		codes[requesttype.Code(code)] = true
	}
	return AppliesTo{codes: codes}, nil
}

// Includes — влияет ли тип заявки на рейтинг.
func (a AppliesTo) Includes(code requesttype.Code) bool {
	return a.all || a.codes[code]
}

// Policy — параметры рейтинга из конфига.
type Policy struct {
	MinVotes       int     // RATING_MIN_VOTES
	ConfirmShare   float64 // RATING_CONFIRM_SHARE, доля «За» строго больше
	RefuteShare    float64 // RATING_REFUTE_SHARE, доля «Против» строго больше
	DeltaConfirmed int     // RATING_DELTA_CONFIRMED
	DeltaRefuted   int     // RATING_DELTA_REFUTED (величина снижения, положительная)
	AppliesTo      AppliesTo
}

// Delta вычисляет изменение рейтинга автора по итогам голосования
// (схема раздела 9). ok = false — рейтинг не меняется.
func (p Policy) Delta(code requesttype.Code, confirms, refutes int) (delta int, reason Reason, ok bool) {
	if !p.AppliesTo.Includes(code) {
		return 0, "", false
	}
	total := confirms + refutes
	if total == 0 || total < p.MinVotes {
		return 0, "", false
	}
	switch {
	case float64(confirms)/float64(total) > p.ConfirmShare:
		delta, reason = p.DeltaConfirmed, ReasonConfirmed
	case float64(refutes)/float64(total) > p.RefuteShare:
		delta, reason = -p.DeltaRefuted, ReasonRefuted
	}
	if delta == 0 {
		return 0, "", false
	}
	return delta, reason, true
}

// Event — изменение рейтинга (rating_events). Рейтинг меняется только
// через события, memberships.rating — кэш их суммы.
type Event struct {
	ChatID    int64
	UserID    int64
	RequestID int64 // 0 для событий без заявки (reset)
	Delta     int
	Reason    Reason
}
