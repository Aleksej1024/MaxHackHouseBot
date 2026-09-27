// Package housechat — домовой чат MAX, в котором состоит бот.
package housechat

// Status — статус домовой чата.
type Status string

const (
	// StatusPending — чат ожидает подтверждения организатора,
	// функции бота для жителей недоступны.
	StatusPending Status = "pending"
	// StatusActive — чат подтверждён, бот работает полноценно.
	StatusActive Status = "active"
	// StatusBotRemoved — бота удалили из чата.
	StatusBotRemoved Status = "bot_removed"
)

// Chat — домовой чат. ID — chat_id в MAX.
type Chat struct {
	ID     int64
	Title  string
	Status Status
}
