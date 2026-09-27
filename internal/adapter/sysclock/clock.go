// Package sysclock — реализация usecase.Clock на системном времени.
package sysclock

import "time"

// Clock возвращает текущее время в UTC.
type Clock struct{}

func (Clock) Now() time.Time { return time.Now().UTC() }
