// Package usecase содержит сценарии приложения и объявляет порты
// (интерфейсы), которые реализуют адаптеры.
package usecase

import (
	"context"
	"errors"
	"time"

	"maxhouse/internal/domain/broadcast"
	"maxhouse/internal/domain/comment"
	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/problem"
	"maxhouse/internal/domain/rating"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/domain/vote"
)

// ErrDMUnavailable — бот не может написать пользователю в личку (постоянная
// ошибка MAX: диалог не начат или бот заблокирован). Не повторяется,
// пользователь помечается недоступным.
var ErrDMUnavailable = errors.New("личные сообщения пользователю недоступны")

// ErrNotFound — запрошенной записи нет в хранилище.
// Оборачивается реализациями репозиториев, проверяется через errors.Is.
var ErrNotFound = errors.New("запись не найдена")

// Clock — источник времени; в тестах заменяется на FakeClock.
type Clock interface {
	Now() time.Time
}

// IDGen — генератор идентификаторов (например request_id для логов).
type IDGen interface {
	NewID() string
}

// FSMStore — хранилище временного состояния диалогов (Redis с TTL).
// Потеря данных допустима: прерывается только незавершённый диалог.
type FSMStore interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}

// TxManager оборачивает работу в транзакцию Postgres,
// чтобы use case не знал о pgx.
type TxManager interface {
	Do(ctx context.Context, fn func(ctx context.Context) error) error
}

// ChatRepo — хранилище домовых чатов.
type ChatRepo interface {
	// Upsert вставляет чат; при конфликте по id обновляет только title,
	// статус существующего чата не меняется.
	Upsert(ctx context.Context, chat housechat.Chat) error
	// Get возвращает чат или ErrNotFound, если его нет.
	Get(ctx context.Context, chatID int64) (housechat.Chat, error)
	// SetStatus меняет статус чата; отсутствие чата — не ошибка.
	SetStatus(ctx context.Context, chatID int64, status housechat.Status) error
}

// MembershipRepo — хранилище пользователей и их участия в чатах.
type MembershipRepo interface {
	// UpsertUser вставляет пользователя; при конфликте обновляет nickname.
	UpsertUser(ctx context.Context, u membership.User) error
	// UpsertMembership вставляет участие; при конфликте по (chat_id, user_id)
	// переводит status в active, а rating, role и joined_at не трогает
	// (повторное добавление бота не обнуляет рейтинги, раздел 6).
	UpsertMembership(ctx context.Context, m membership.Membership) error
	// SetStatus меняет статус участия; отсутствие записи — не ошибка.
	SetStatus(ctx context.Context, chatID, userID int64, status membership.Status) error
}

// MemberDirectory — чтение участия пользователя в чатах для сценариев в личке
type MemberDirectory interface {
	// UpdateNickname обновляет ник известного пользователя;
	// неизвестный пользователь — не ошибка, запись не создаётся.
	UpdateNickname(ctx context.Context, userID int64, nickname string) error
	// ListActiveChats возвращает чаты, где пользователь — активный участник
	// и сам чат в статусе active (pending выглядит как отсутствие чата).
	// Порядок стабильный: по title, затем по id.
	ListActiveChats(ctx context.Context, userID int64) ([]housechat.Chat, error)
}

// MembershipReader — чтение участия для проверок прав в сценариях заявок.
type MembershipReader interface {
	// GetActiveForUpdate возвращает участие активного участника активного
	// чата и блокирует строку до конца транзакции (защита лимита от гонок).
	// Если пользователь не участник или чат не active — ErrNotFound.
	GetActiveForUpdate(ctx context.Context, chatID, userID int64) (membership.Membership, error)
}

// ChatSyncState — отметки синхронизации заявки с сообщением в чате.
type ChatSyncState interface {
	// SetChatMessage сохраняет id сообщения в чате и chat_sync = ok.
	SetChatMessage(ctx context.Context, requestID int64, messageID string) error
	// SetChatSyncFailed помечает, что сообщение в чате не синхронизировано.
	SetChatSyncFailed(ctx context.Context, requestID int64) error
}

// RequestRepo — хранилище заявок.
type RequestRepo interface {
	ChatSyncState
	// CountCreatedSince — число заявок автора этого типа в чате, созданных
	// не раньше since (включая удалённые, чтобы удаление не обходило лимит).
	CountCreatedSince(ctx context.Context, chatID, authorID int64, typ requesttype.Code, since time.Time) (int, error)
	// Create сохраняет заявку и ссылки на вложения, возвращает id заявки.
	Create(ctx context.Context, r request.Request, atts []request.Attachment) (int64, error)
}

// RequestLocker — чтение заявки с блокировкой строки до конца транзакции:
// сериализует голоса и переходы статусов одной заявки.
type RequestLocker interface {
	// GetForUpdate возвращает заявку или ErrNotFound.
	GetForUpdate(ctx context.Context, requestID int64) (request.Request, error)
	// ClaimRating атомарно ставит rating_applied = true; false — рейтинг
	// по заявке уже посчитан (идемпотентность пересчёта, раздел 9).
	ClaimRating(ctx context.Context, requestID int64) (bool, error)
}

// RequestLifecycleRepo — изменения заявок по жизненному циклу (раздел 8.1, 8.7).
type RequestLifecycleRepo interface {
	// NextVotingEnded блокирует (FOR UPDATE SKIP LOCKED) одну заявку open,
	// у которой закончилось голосование, но не истёк общий срок.
	NextVotingEnded(ctx context.Context, now time.Time) (request.Request, bool, error)
	// NextExpired блокирует (FOR UPDATE SKIP LOCKED) одну заявку open или
	// in_progress с истёкшим общим сроком.
	NextExpired(ctx context.Context, now time.Time) (request.Request, bool, error)
	// SaveState сохраняет status, result_text и closed_at.
	SaveState(ctx context.Context, r request.Request) error
	// UpdateBody меняет текст заявки (голоса не сбрасываются).
	UpdateBody(ctx context.Context, requestID int64, body string) error
	// ListByAuthor — заявки автора в чате, кроме удалённых, новые первыми.
	ListByAuthor(ctx context.Context, chatID, authorID int64, limit int) ([]request.Request, error)
}

// VoteRepo — голоса по заявкам. Голос уникален по (request_id, user_id).
type VoteRepo interface {
	// Upsert ставит или меняет голос и возвращает предыдущее значение
	// (пустое, если голоса не было).
	Upsert(ctx context.Context, requestID, userID int64, value vote.Value) (prev vote.Value, err error)
	// Count возвращает число голосов «За» и «Против».
	Count(ctx context.Context, requestID int64) (confirms, refutes int, err error)
}

// RatingRepo — изменения рейтинга. AddEvent пишет rating_events и в той же
// транзакции обновляет кэш memberships.rating.
type RatingRepo interface {
	AddEvent(ctx context.Context, e rating.Event) error
}

// RatingResetRepo — периодический сброс отрицательных рейтингов (раздел 9).
type RatingResetRepo interface {
	// ResetNegative обнуляет отрицательные рейтинги во всех чатах, пишет
	// события reset и возвращает число изменённых участий.
	ResetNegative(ctx context.Context) (int, error)
}

// JobRuns — журнал запусков периодических задач планировщика.
type JobRuns interface {
	// TryStart блокирует запись задачи и, если последний запуск был не позже
	// notAfter (или запусков не было), отмечает запуск в now и возвращает true.
	// Вызывать в транзакции.
	TryStart(ctx context.Context, job string, notAfter, now time.Time) (bool, error)
}

// CardReader собирает актуальную карточку заявки из БД: заявка, вложения,
// ник автора (пустой для анонимной), счётчики голосов и материалов.
type CardReader interface {
	LoadCard(ctx context.Context, requestID int64) (RequestCard, error)
}

// RequestCard — всё, что нужно для отображения заявки в домовом чате.
// AuthorNickname пуст для анонимной заявки: автор не уходит в MAX вовсе.
type RequestCard struct {
	Request        request.Request
	Attachments    []request.Attachment
	AuthorNickname string
	Confirms       int
	Refutes        int
	Materials      int
}

// ChatPublisher — публикация карточек заявок в домовом чате. Реализация
// в адаптере MAX оформляет текст и кнопки и повторяет временные ошибки.
type ChatPublisher interface {
	PublishRequest(ctx context.Context, card RequestCard) (messageID string, err error)
	// EditRequest обновляет опубликованную карточку (card.Request.ChatMessageID).
	// Кнопки голосования остаются, только пока голосование идёт.
	EditRequest(ctx context.Context, card RequestCard) error
	// DeleteRequest удаляет сообщение заявки в чате.
	DeleteRequest(ctx context.Context, messageID string) error
	// PublishResult публикует в чате итог закрытой заявки ответом на карточку.
	PublishResult(ctx context.Context, card RequestCard) error
}

// RequestBrowser — чтение заявок для жителей.
type RequestBrowser interface {
	// ListActiveInChat — заявки open и in_progress в чате, новые первыми.
	ListActiveInChat(ctx context.Context, chatID int64, limit int) ([]request.Request, error)
}

// CommentRepo — комментарии и доказательства (раздел 8.4).
type CommentRepo interface {
	// Add сохраняет материал и его вложения, возвращает id.
	Add(ctx context.Context, c comment.Comment) (int64, error)
	// List — страница материалов заявки по времени, с ником отправителя.
	List(ctx context.Context, requestID int64, offset, limit int) ([]comment.Comment, error)
	// Count — всего материалов у заявки.
	Count(ctx context.Context, requestID int64) (int, error)
}

// BroadcastRepo — рассылки по заявкам (раздел 8.5).
type BroadcastRepo interface {
	// LastAt — время последней рассылки по заявке; found = false, если не было.
	LastAt(ctx context.Context, requestID int64) (at time.Time, found bool, err error)
	// Record записывает факт рассылки.
	Record(ctx context.Context, requestID int64, audience broadcast.Audience, recipients int, at time.Time) error
	// Recipients — проголосовавшие из аудитории, которым бот может писать
	// в личку (dm_unavailable = false).
	Recipients(ctx context.Context, requestID int64, audience broadcast.Audience) ([]int64, error)
}

// MemberSettings — настройки жителя в домовом чате.
type MemberSettings interface {
	// GetMembership — участие активного участника активного чата или ErrNotFound.
	GetMembership(ctx context.Context, chatID, userID int64) (membership.Membership, error)
	SetForwardToDM(ctx context.Context, chatID, userID int64, on bool) error
	SetNotifyMaterials(ctx context.Context, chatID, userID int64, on bool) error
	// ForwardRecipients — активные участники чата с включённой пересылкой,
	// доступные в личке, кроме exceptUserID.
	ForwardRecipients(ctx context.Context, chatID, exceptUserID int64) ([]int64, error)
}

// DMAvailability — отметка недоступности лички пользователя.
type DMAvailability interface {
	MarkDMUnavailable(ctx context.Context, userID int64) error
}

// UserNotifier — сообщения жителям в личку. Тексты оформляет адаптер.
// Постоянную невозможность написать пользователю реализация возвращает
// как ErrDMUnavailable.
type UserNotifier interface {
	// SendBroadcast — сообщение автора заявки жителю (автор не раскрывается).
	SendBroadcast(ctx context.Context, userID int64, req request.Request, text string) error
	// NotifyMaterial — автору заявки о новом комментарии или доказательстве
	// (если у автора включена настройка NotifyMaterials).
	NotifyMaterial(ctx context.Context, authorID int64, req request.Request, c comment.Comment) error
	// ForwardRequest — новая заявка чата жителю, включившему пересылку.
	ForwardRequest(ctx context.Context, userID int64, card RequestCard) error
}

// ProblemRepo — инциденты по сообщениям о проблемах (раздел 8.8).
type ProblemRepo interface {
	// Lock сериализует обработку одной проблемы в одном доме до конца
	// транзакции: параллельные сообщения не создадут два окна и не отправят
	// два оповещения. Вызывать в транзакции.
	Lock(ctx context.Context, chatID int64, code problem.Code) error
	// Current — последнее окно по проблеме в доме, начатое позже openSince,
	// с числом сообщивших жителей.
	Current(ctx context.Context, chatID int64, code problem.Code, openSince time.Time) (problem.Incident, bool, error)
	// Create сохраняет новое окно (без жителей) и возвращает id.
	Create(ctx context.Context, i problem.Incident) (int64, error)
	// AddReporter записывает, что житель сообщил о проблеме в окне;
	// false — он уже сообщал в этом окне.
	AddReporter(ctx context.Context, incidentID, userID int64, at time.Time) (bool, error)
	// SetNotified ставит отметку об оповещении; нулевое at — снимает её.
	SetNotified(ctx context.Context, incidentID int64, at time.Time) error
}

// ChatAdmins — администраторы домового чата из БД (роль с момента
// добавления бота). Запасной источник, если MAX не ответил.
type ChatAdmins interface {
	ListChatAdmins(ctx context.Context, chatID int64) ([]membership.User, error)
}

// ProblemAlerter публикует в домовом чате оповещение о массовой проблеме
// с упоминанием администраторов. Тексты оформляет адаптер.
type ProblemAlerter interface {
	SendProblemAlert(ctx context.Context, chatID int64, code problem.Code, admins []membership.User) error
}

// MemberLister — порт к MAX API: сведения о домовом чате.
type MemberLister interface {
	// ListMembers — полный список участников чата, пагинация скрыта в реализации.
	ListMembers(ctx context.Context, chatID int64) ([]membership.Member, error)
	// ChatTitle — название чата (в событии bot_added MAX его не присылает).
	ChatTitle(ctx context.Context, chatID int64) (string, error)
}

// Messenger — порт исходящих сообщений MAX.
// Реализация в адаптере оборачивает вызовы в повторы.
type Messenger interface {
	// SendChatMessage отправляет сообщение в чат и возвращает его id в MAX.
	SendChatMessage(ctx context.Context, chatID int64, text string) (messageID string, err error)
}
