// Package membership — пользователи MAX и их участие в домовых чатах
// Рейтинг привязан к паре (пользователь, чат).
package membership

// Role — роль участника в чате. Прав внутри бота не даёт.
type Role string

const (
	RoleMember Role = "member"
	RoleAdmin  Role = "admin"
)

// Status — статус участия в чате.
type Status string

const (
	StatusActive Status = "active"
	StatusLeft   Status = "left"
)

// User — пользователь MAX. Храним только id и ник (раздел 3, п. 8).
type User struct {
	ID       int64
	Nickname string
}

// Membership — участие пользователя в домовой чате.
// Rating — кэш суммы RATING_EVENT для этой пары.
type Membership struct {
	ChatID int64
	UserID int64
	Role   Role
	Rating int
	Status Status
	// ForwardToDM — присылать новые заявки дома в личку (раздел 8.6).
	ForwardToDM bool
	// NotifyMaterials — уведомлять о комментариях и доказательствах к своим
	// заявкам (раздел 8.4). В БД по умолчанию true.
	NotifyMaterials bool
}

// Member — элемент списка участников чата, полученного из MAX API.
type Member struct {
	UserID   int64
	Nickname string
	IsAdmin  bool // IsAdmin || IsOwner в MAX
	IsBot    bool
}
