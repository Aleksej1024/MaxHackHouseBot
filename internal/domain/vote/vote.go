// Package vote — голос жителя по заявке.
// В БД значение всегда confirm/refute, подписи кнопок зависят от типа заявки.
package vote

// Value — значение голоса.
type Value string

const (
	Confirm Value = "confirm" // «За»: «Подтверждаю», в опросе «Да»
	Refute  Value = "refute"  // «Против»: «Опровергаю», в опросе «Нет»
)

// Parse разбирает значение голоса; ok = false для неизвестного значения.
func Parse(s string) (Value, bool) {
	switch Value(s) {
	case Confirm, Refute:
		return Value(s), true
	}
	return "", false
}
