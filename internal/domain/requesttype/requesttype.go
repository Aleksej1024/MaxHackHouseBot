// Package requesttype — реестр типов заявок.
//
// Реестр хранит только коды и необязательные переопределения сроков.
// Названия типов и подписи кнопок — тексты интерфейса, они лежат
// в adapter/maxbot/texts.go по коду типа (раздел 15). Чтобы добавить тип,
// достаточно записи здесь и текстов там; миграция схемы не нужна.
package requesttype

import "time"

// Code — код типа заявки (request.type в БД).
type Code string

const (
	Complaint  Code = "complaint"
	Initiative Code = "initiative"
	Poll       Code = "poll"
)

// Type — описание типа заявки. Нулевые длительности означают
// значения из конфига (VOTING_DURATION, REQUEST_TTL_DEFAULT).
type Type struct {
	Code           Code
	VotingDuration time.Duration
	TTL            time.Duration
}

// Registry — упорядоченный реестр типов.
type Registry struct {
	types  []Type
	byCode map[Code]Type
}

// New создаёт реестр; порядок типов сохраняется (порядок кнопок выбора).
func New(types ...Type) *Registry {
	r := &Registry{byCode: make(map[Code]Type, len(types))}
	for _, t := range types {
		if _, dup := r.byCode[t.Code]; dup {
			continue
		}
		r.types = append(r.types, t)
		r.byCode[t.Code] = t
	}
	return r
}

// Default — реестр по умолчанию: жалоба, инициатива.
//
// Опрос временно отключён (решение владельца проекта): без записи в реестре
// нет кнопки выбора типа, и use case не примет его код. Константа Poll и
// тексты в texts.go остаются — по уже созданным опросам карточки и голосование
// работают как раньше. Чтобы вернуть опрос, раскомментируйте строку.
func Default() *Registry {
	return New(
		Type{Code: Complaint},
		Type{Code: Initiative},
		// Type{Code: Poll},
	)
}

// Get возвращает тип по коду.
func (r *Registry) Get(code Code) (Type, bool) {
	t, ok := r.byCode[code]
	return t, ok
}

// All возвращает копию списка типов в порядке регистрации.
func (r *Registry) All() []Type {
	out := make([]Type, len(r.types))
	copy(out, r.types)
	return out
}

// Durations возвращает срок голосования и общий срок жизни заявки
// с учётом переопределений типа.
func (t Type) Durations(defaultVoting, defaultTTL time.Duration) (voting, ttl time.Duration) {
	voting, ttl = defaultVoting, defaultTTL
	if t.VotingDuration > 0 {
		voting = t.VotingDuration
	}
	if t.TTL > 0 {
		ttl = t.TTL
	}
	return voting, ttl
}
