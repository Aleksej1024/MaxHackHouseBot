// Package problem — сообщения жителей о бытовых проблемах в доме:
//  нет интернета, пропала вода и т. п. Это не заявка: без
// голосования, рейтинга, лимитов и материалов.
//
// Сообщения копятся в «инциденте» — окне с первого сообщения о проблеме
// в доме. Когда в окне набирается Threshold разных жителей, в домовой чат
// один раз уходит оповещение. После окна счёт начинается заново.
package problem

import (
	"slices"
	"time"
)

// Code — код проблемы (problem_reports.problem). Названия для жителей —
// в adapter/maxbot/texts.go (ProblemNames), как у типов заявок.
type Code string

const (
	NoInternet Code = "no_internet"
	NoPower    Code = "no_power"
	NoWater    Code = "no_water"
	NoGas      Code = "no_gas"
	Drone      Code = "drone"
)

// Codes — проблемы в порядке кнопок. Чтобы добавить проблему, достаточно
// кода здесь и названия в texts.go; миграция не нужна.
var Codes = []Code{NoInternet, NoPower, NoWater, NoGas, Drone}

// Known — есть ли код в списке (данным кнопки не доверяем).
func Known(c Code) bool {
	return slices.Contains(Codes, c)
}

// Policy — порог оповещения из конфига.
type Policy struct {
	Threshold int           // PROBLEM_ALERT_THRESHOLD: сколько разных жителей
	Window    time.Duration // PROBLEM_ALERT_WINDOW: окно с первого сообщения
}

// OpenSince — окна, начатые позже этого момента, ещё открыты.
func (p Policy) OpenSince(now time.Time) time.Time {
	return now.Add(-p.Window)
}

// Incident — сообщения об одной проблеме в одном доме в пределах окна.
// Сами жители хранятся отдельно (по строке на жителя), здесь — их число.
type Incident struct {
	ID              int64
	ChatID          int64
	Problem         Code
	FirstReportedAt time.Time
	Reporters       int // сколько разных жителей сообщили в окне
	Notified        bool
	NotifiedAt      time.Time // нулевое, пока оповещения не было
}

// New — первое сообщение открывает окно. Жителя добавляет AddReporter.
func New(chatID int64, code Code, now time.Time) Incident {
	return Incident{ChatID: chatID, Problem: code, FirstReportedAt: now}
}

// AddReporter учитывает нового жителя. Повтор того же жителя отсекает
// хранилище (житель в окне один раз), сюда он не доходит.
func (i *Incident) AddReporter() {
	i.Reporters++
}

// NeedsAlert — набрался порог, а оповещения в этом окне ещё не было.
func (i Incident) NeedsAlert(p Policy) bool {
	return !i.Notified && i.Reporters >= p.Threshold
}

// MarkNotified отмечает отправку оповещения.
func (i *Incident) MarkNotified(now time.Time) {
	i.Notified, i.NotifiedAt = true, now
}

// ClearNotified снимает отметку, если оповещение отправить не удалось.
func (i *Incident) ClearNotified() {
	i.Notified, i.NotifiedAt = false, time.Time{}
}
