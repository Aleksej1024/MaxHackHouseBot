package e2e

// In-memory реализации всех портов хранения.
// Семантика повторяет SQL адаптера postgres, проверенного интеграционными
// тестами: фильтры по статусам, порядок, флаги. Транзакции не откатываются —
// сценарии в e2e однопоточные, откат проверяется интеграционными тестами.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"maxhouse/internal/app"
	"maxhouse/internal/domain/broadcast"
	"maxhouse/internal/domain/comment"
	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/problem"
	"maxhouse/internal/domain/rating"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/domain/vote"
	"maxhouse/internal/usecase"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type memberKey struct{ chatID, userID int64 }

type storedUser struct {
	membership.User
	dmUnavailable bool
}

type storedRequest struct {
	request.Request
	chatSync      string
	ratingApplied bool
	atts          []request.Attachment
}

type storedBroadcast struct {
	requestID int64
	audience  broadcast.Audience
	at        time.Time
}

// memDB — общее состояние; отдельные типы ниже реализуют порты с
// пересекающимися именами методов (SetStatus у чатов и участников).
type memDB struct {
	mu          sync.Mutex
	clock       *fakeClock
	chats       map[int64]housechat.Chat
	users       map[int64]*storedUser
	members     map[memberKey]*membership.Membership
	requests    map[int64]*storedRequest
	votes       map[int64]map[int64]vote.Value
	events      []rating.Event
	comments    []comment.Comment
	broadcasts  []storedBroadcast
	problems    []problem.Incident
	reporters   map[int64]map[int64]time.Time // окно → житель → время сообщения
	jobRuns     map[string]time.Time
	fsm         map[string]string
	fsmExpires  map[string]time.Time
	nextRequest int64
}

func newMemDB(clock *fakeClock) *memDB {
	return &memDB{
		clock:      clock,
		chats:      map[int64]housechat.Chat{},
		users:      map[int64]*storedUser{},
		members:    map[memberKey]*membership.Membership{},
		requests:   map[int64]*storedRequest{},
		votes:      map[int64]map[int64]vote.Value{},
		jobRuns:    map[string]time.Time{},
		reporters:  map[int64]map[int64]time.Time{},
		fsm:        map[string]string{},
		fsmExpires: map[string]time.Time{},
	}
}

// adapters возвращает порты для app.Build (кроме мессенджера и MemberLister).
func (db *memDB) adapters() app.Adapters {
	return app.Adapters{
		Chats:       memChats{db},
		Memberships: memMembers{db},
		Requests:    memRequests{db},
		Votes:       memVotes{db},
		Ratings:     memRatings{db},
		JobRuns:     memJobRuns{db},
		Comments:    memComments{db},
		Broadcasts:  memBroadcasts{db},
		Problems:    memProblems{db},
		FSM:         memFSM{db},
		Tx:          memTx{},
		Clock:       db.clock,
	}
}

func (db *memDB) rating(chatID, userID int64) int {
	db.mu.Lock()
	defer db.mu.Unlock()
	if m := db.members[memberKey{chatID, userID}]; m != nil {
		return m.Rating
	}
	return 0
}

type memTx struct{}

func (memTx) Do(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

// --- FSMStore ---

type memFSM struct{ db *memDB }

func (s memFSM) Get(_ context.Context, key string) (string, bool, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	v, ok := s.db.fsm[key]
	if !ok || !s.db.clock.Now().Before(s.db.fsmExpires[key]) {
		return "", false, nil
	}
	return v, true, nil
}

func (s memFSM) Set(_ context.Context, key, value string, ttl time.Duration) error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	s.db.fsm[key] = value
	s.db.fsmExpires[key] = s.db.clock.Now().Add(ttl)
	return nil
}

func (s memFSM) Delete(_ context.Context, key string) error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	delete(s.db.fsm, key)
	delete(s.db.fsmExpires, key)
	return nil
}

// --- Чаты ---

type memChats struct{ db *memDB }

func (c memChats) Upsert(_ context.Context, chat housechat.Chat) error {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	if existing, ok := c.db.chats[chat.ID]; ok {
		existing.Title = chat.Title
		c.db.chats[chat.ID] = existing
		return nil
	}
	c.db.chats[chat.ID] = chat
	return nil
}

func (c memChats) Get(_ context.Context, chatID int64) (housechat.Chat, error) {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	chat, ok := c.db.chats[chatID]
	if !ok {
		return housechat.Chat{}, usecase.ErrNotFound
	}
	return chat, nil
}

func (c memChats) SetStatus(_ context.Context, chatID int64, status housechat.Status) error {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	if chat, ok := c.db.chats[chatID]; ok {
		chat.Status = status
		c.db.chats[chatID] = chat
	}
	return nil
}

// --- Пользователи и участие ---

type memMembers struct{ db *memDB }

func (m memMembers) UpsertUser(_ context.Context, u membership.User) error {
	m.db.mu.Lock()
	defer m.db.mu.Unlock()
	if su, ok := m.db.users[u.ID]; ok {
		su.Nickname = u.Nickname
		return nil
	}
	m.db.users[u.ID] = &storedUser{User: u}
	return nil
}

func (m memMembers) UpsertMembership(_ context.Context, ms membership.Membership) error {
	m.db.mu.Lock()
	defer m.db.mu.Unlock()
	key := memberKey{ms.ChatID, ms.UserID}
	if existing, ok := m.db.members[key]; ok {
		existing.Status = membership.StatusActive
		return nil
	}
	cp := ms
	cp.NotifyMaterials = true // DEFAULT true в БД
	m.db.members[key] = &cp
	return nil
}

func (m memMembers) SetStatus(_ context.Context, chatID, userID int64, status membership.Status) error {
	m.db.mu.Lock()
	defer m.db.mu.Unlock()
	if ms, ok := m.db.members[memberKey{chatID, userID}]; ok {
		ms.Status = status
	}
	return nil
}

func (m memMembers) UpdateNickname(_ context.Context, userID int64, nickname string) error {
	m.db.mu.Lock()
	defer m.db.mu.Unlock()
	if su, ok := m.db.users[userID]; ok {
		su.Nickname = nickname
		su.dmUnavailable = false
	}
	return nil
}

func (m memMembers) ListActiveChats(_ context.Context, userID int64) ([]housechat.Chat, error) {
	m.db.mu.Lock()
	defer m.db.mu.Unlock()
	var out []housechat.Chat
	for key, ms := range m.db.members {
		chat := m.db.chats[key.chatID]
		if key.userID == userID && ms.Status == membership.StatusActive && chat.Status == housechat.StatusActive {
			out = append(out, chat)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Title != out[j].Title {
			return out[i].Title < out[j].Title
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m memMembers) activeMember(chatID, userID int64) (membership.Membership, error) {
	ms, ok := m.db.members[memberKey{chatID, userID}]
	if !ok || ms.Status != membership.StatusActive || m.db.chats[chatID].Status != housechat.StatusActive {
		return membership.Membership{}, usecase.ErrNotFound
	}
	return *ms, nil
}

func (m memMembers) GetActiveForUpdate(_ context.Context, chatID, userID int64) (membership.Membership, error) {
	m.db.mu.Lock()
	defer m.db.mu.Unlock()
	return m.activeMember(chatID, userID)
}

func (m memMembers) GetMembership(_ context.Context, chatID, userID int64) (membership.Membership, error) {
	m.db.mu.Lock()
	defer m.db.mu.Unlock()
	return m.activeMember(chatID, userID)
}

func (m memMembers) SetForwardToDM(_ context.Context, chatID, userID int64, on bool) error {
	m.db.mu.Lock()
	defer m.db.mu.Unlock()
	if ms, ok := m.db.members[memberKey{chatID, userID}]; ok {
		ms.ForwardToDM = on
	}
	return nil
}

func (m memMembers) SetNotifyMaterials(_ context.Context, chatID, userID int64, on bool) error {
	m.db.mu.Lock()
	defer m.db.mu.Unlock()
	if ms, ok := m.db.members[memberKey{chatID, userID}]; ok {
		ms.NotifyMaterials = on
	}
	return nil
}

func (m memMembers) ForwardRecipients(_ context.Context, chatID, exceptUserID int64) ([]int64, error) {
	m.db.mu.Lock()
	defer m.db.mu.Unlock()
	var ids []int64
	for key, ms := range m.db.members {
		u := m.db.users[key.userID]
		if key.chatID == chatID && ms.ForwardToDM && ms.Status == membership.StatusActive &&
			key.userID != exceptUserID && u != nil && !u.dmUnavailable {
			ids = append(ids, key.userID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func (m memMembers) MarkDMUnavailable(_ context.Context, userID int64) error {
	m.db.mu.Lock()
	defer m.db.mu.Unlock()
	if su, ok := m.db.users[userID]; ok {
		su.dmUnavailable = true
	}
	return nil
}

// --- Заявки ---

type memRequests struct{ db *memDB }

func (r memRequests) CountCreatedSince(_ context.Context, chatID, authorID int64, typ requesttype.Code, since time.Time) (int, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	n := 0
	for _, sr := range r.db.requests {
		if sr.ChatID == chatID && sr.AuthorID == authorID && sr.Type == typ && !sr.CreatedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

func (r memRequests) Create(_ context.Context, req request.Request, atts []request.Attachment) (int64, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	r.db.nextRequest++
	req.ID = r.db.nextRequest
	r.db.requests[req.ID] = &storedRequest{Request: req, chatSync: "ok", atts: append([]request.Attachment(nil), atts...)}
	return req.ID, nil
}

func (r memRequests) SetChatMessage(_ context.Context, id int64, mid string) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	if sr, ok := r.db.requests[id]; ok {
		sr.ChatMessageID, sr.chatSync = mid, "ok"
	}
	return nil
}

func (r memRequests) SetChatSyncFailed(_ context.Context, id int64) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	if sr, ok := r.db.requests[id]; ok {
		sr.chatSync = "failed"
	}
	return nil
}

func (r memRequests) GetForUpdate(_ context.Context, id int64) (request.Request, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	sr, ok := r.db.requests[id]
	if !ok {
		return request.Request{}, usecase.ErrNotFound
	}
	return sr.Request, nil
}

func (r memRequests) ClaimRating(_ context.Context, id int64) (bool, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	sr, ok := r.db.requests[id]
	if !ok || sr.ratingApplied {
		return false, nil
	}
	sr.ratingApplied = true
	return true, nil
}

func (r memRequests) next(match func(*storedRequest) bool, less func(a, b *storedRequest) bool) (request.Request, bool, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	var best *storedRequest
	for _, sr := range r.db.requests {
		if match(sr) && (best == nil || less(sr, best)) {
			best = sr
		}
	}
	if best == nil {
		return request.Request{}, false, nil
	}
	return best.Request, true, nil
}

func (r memRequests) NextVotingEnded(_ context.Context, now time.Time) (request.Request, bool, error) {
	return r.next(func(sr *storedRequest) bool {
		return sr.Status == request.StatusOpen && !sr.VotingEndsAt.After(now) && sr.ExpiresAt.After(now)
	}, func(a, b *storedRequest) bool { return earlier(a.VotingEndsAt, b.VotingEndsAt, a.ID, b.ID) })
}

func (r memRequests) NextExpired(_ context.Context, now time.Time) (request.Request, bool, error) {
	return r.next(func(sr *storedRequest) bool {
		return sr.IsActive() && !sr.ExpiresAt.After(now)
	}, func(a, b *storedRequest) bool { return earlier(a.ExpiresAt, b.ExpiresAt, a.ID, b.ID) })
}

// earlier — порядок ORDER BY <срок>, id.
func earlier(a, b time.Time, idA, idB int64) bool {
	if !a.Equal(b) {
		return a.Before(b)
	}
	return idA < idB
}

func (r memRequests) SaveState(_ context.Context, req request.Request) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	if sr, ok := r.db.requests[req.ID]; ok {
		sr.Status, sr.ResultText, sr.ClosedAt = req.Status, req.ResultText, req.ClosedAt
	}
	return nil
}

func (r memRequests) UpdateBody(_ context.Context, id int64, body string) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	if sr, ok := r.db.requests[id]; ok {
		sr.Body = body
	}
	return nil
}

func (r memRequests) list(match func(*storedRequest) bool, limit int) []request.Request {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	var out []request.Request
	for _, sr := range r.db.requests {
		if match(sr) {
			out = append(out, sr.Request)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (r memRequests) ListByAuthor(_ context.Context, chatID, authorID int64, limit int) ([]request.Request, error) {
	return r.list(func(sr *storedRequest) bool {
		return sr.ChatID == chatID && sr.AuthorID == authorID && sr.Status != request.StatusDeleted
	}, limit), nil
}

func (r memRequests) ListActiveInChat(_ context.Context, chatID int64, limit int) ([]request.Request, error) {
	return r.list(func(sr *storedRequest) bool { return sr.ChatID == chatID && sr.IsActive() }, limit), nil
}

func (r memRequests) LoadCard(_ context.Context, id int64) (usecase.RequestCard, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	sr, ok := r.db.requests[id]
	if !ok {
		return usecase.RequestCard{}, usecase.ErrNotFound
	}
	card := usecase.RequestCard{Request: sr.Request, Attachments: append([]request.Attachment(nil), sr.atts...)}
	if !sr.IsAnonymous {
		if u := r.db.users[sr.AuthorID]; u != nil {
			card.AuthorNickname = u.Nickname
		}
	}
	for _, v := range r.db.votes[id] {
		if v == vote.Confirm {
			card.Confirms++
		} else {
			card.Refutes++
		}
	}
	for _, c := range r.db.comments {
		if c.RequestID == id {
			card.Materials++
		}
	}
	return card, nil
}

// --- Голоса и рейтинг ---

type memVotes struct{ db *memDB }

func (v memVotes) Upsert(_ context.Context, requestID, userID int64, value vote.Value) (vote.Value, error) {
	v.db.mu.Lock()
	defer v.db.mu.Unlock()
	if v.db.votes[requestID] == nil {
		v.db.votes[requestID] = map[int64]vote.Value{}
	}
	prev := v.db.votes[requestID][userID]
	v.db.votes[requestID][userID] = value
	return prev, nil
}

func (v memVotes) Count(_ context.Context, requestID int64) (int, int, error) {
	v.db.mu.Lock()
	defer v.db.mu.Unlock()
	var c, r int
	for _, val := range v.db.votes[requestID] {
		if val == vote.Confirm {
			c++
		} else {
			r++
		}
	}
	return c, r, nil
}

type memRatings struct{ db *memDB }

func (r memRatings) AddEvent(_ context.Context, e rating.Event) error {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	r.db.events = append(r.db.events, e)
	if ms, ok := r.db.members[memberKey{e.ChatID, e.UserID}]; ok {
		ms.Rating += e.Delta
	}
	return nil
}

func (r memRatings) ResetNegative(_ context.Context) (int, error) {
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	n := 0
	for key, ms := range r.db.members {
		if ms.Rating < 0 {
			r.db.events = append(r.db.events, rating.Event{ChatID: key.chatID, UserID: key.userID, Delta: -ms.Rating, Reason: rating.ReasonReset})
			ms.Rating = 0
			n++
		}
	}
	return n, nil
}

type memJobRuns struct{ db *memDB }

func (j memJobRuns) TryStart(_ context.Context, job string, notAfter, now time.Time) (bool, error) {
	j.db.mu.Lock()
	defer j.db.mu.Unlock()
	if last, ok := j.db.jobRuns[job]; ok && last.After(notAfter) {
		return false, nil
	}
	j.db.jobRuns[job] = now
	return true, nil
}

// --- Материалы и рассылки ---

type memComments struct{ db *memDB }

func (c memComments) Add(_ context.Context, cm comment.Comment) (int64, error) {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	cm.ID = int64(len(c.db.comments) + 1)
	c.db.comments = append(c.db.comments, cm)
	return cm.ID, nil
}

func (c memComments) List(_ context.Context, requestID int64, offset, limit int) ([]comment.Comment, error) {
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	var all []comment.Comment
	for _, cm := range c.db.comments {
		if cm.RequestID == requestID {
			if u := c.db.users[cm.AuthorID]; u != nil {
				cm.AuthorNickname = u.Nickname
			}
			all = append(all, cm)
		}
	}
	if offset >= len(all) {
		return nil, nil
	}
	return all[offset:min(offset+limit, len(all))], nil
}

func (c memComments) Count(ctx context.Context, requestID int64) (int, error) {
	all, err := c.List(ctx, requestID, 0, 1<<30)
	return len(all), err
}

type memBroadcasts struct{ db *memDB }

func (b memBroadcasts) LastAt(_ context.Context, requestID int64) (time.Time, bool, error) {
	b.db.mu.Lock()
	defer b.db.mu.Unlock()
	var last time.Time
	found := false
	for _, bc := range b.db.broadcasts {
		if bc.requestID == requestID && (!found || bc.at.After(last)) {
			last, found = bc.at, true
		}
	}
	return last, found, nil
}

func (b memBroadcasts) Record(_ context.Context, requestID int64, aud broadcast.Audience, _ int, at time.Time) error {
	b.db.mu.Lock()
	defer b.db.mu.Unlock()
	b.db.broadcasts = append(b.db.broadcasts, storedBroadcast{requestID: requestID, audience: aud, at: at})
	return nil
}

func (b memBroadcasts) Recipients(_ context.Context, requestID int64, aud broadcast.Audience) ([]int64, error) {
	b.db.mu.Lock()
	defer b.db.mu.Unlock()
	sr, ok := b.db.requests[requestID]
	if !ok {
		return nil, fmt.Errorf("заявка %d не найдена", requestID)
	}
	wanted := map[vote.Value]bool{}
	for _, v := range aud.Votes() {
		wanted[v] = true
	}
	var ids []int64
	for uid, v := range b.db.votes[requestID] {
		ms := b.db.members[memberKey{sr.ChatID, uid}]
		u := b.db.users[uid]
		if wanted[v] && ms != nil && ms.Status == membership.StatusActive && u != nil && !u.dmUnavailable {
			ids = append(ids, uid)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// ListChatAdmins — активные участники с ролью admin, по возрастанию id.
func (m memMembers) ListChatAdmins(_ context.Context, chatID int64) ([]membership.User, error) {
	m.db.mu.Lock()
	defer m.db.mu.Unlock()
	var out []membership.User
	for key, ms := range m.db.members {
		if key.chatID == chatID && ms.Role == membership.RoleAdmin && ms.Status == membership.StatusActive {
			out = append(out, m.db.users[key.userID].User)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// --- ProblemRepo ---

type memProblems struct{ db *memDB }

// Lock — сценарии однопоточные, блокировка проверяется интеграционными тестами.
func (memProblems) Lock(context.Context, int64, problem.Code) error { return nil }

func (p memProblems) Current(_ context.Context, chatID int64, code problem.Code, openSince time.Time) (problem.Incident, bool, error) {
	p.db.mu.Lock()
	defer p.db.mu.Unlock()
	for i := len(p.db.problems) - 1; i >= 0; i-- {
		inc := p.db.problems[i]
		if inc.ChatID == chatID && inc.Problem == code && inc.FirstReportedAt.After(openSince) {
			inc.Reporters = len(p.db.reporters[inc.ID])
			return inc, true, nil
		}
	}
	return problem.Incident{}, false, nil
}

func (p memProblems) Create(_ context.Context, i problem.Incident) (int64, error) {
	p.db.mu.Lock()
	defer p.db.mu.Unlock()
	i.ID = int64(len(p.db.problems) + 1)
	p.db.problems = append(p.db.problems, i)
	return i.ID, nil
}

// AddReporter — как первичный ключ (report_id, user_id): житель в окне один раз.
func (p memProblems) AddReporter(_ context.Context, incidentID, userID int64, at time.Time) (bool, error) {
	p.db.mu.Lock()
	defer p.db.mu.Unlock()
	users := p.db.reporters[incidentID]
	if users == nil {
		users = map[int64]time.Time{}
		p.db.reporters[incidentID] = users
	}
	if _, ok := users[userID]; ok {
		return false, nil
	}
	users[userID] = at
	return true, nil
}

func (p memProblems) SetNotified(_ context.Context, incidentID int64, at time.Time) error {
	p.db.mu.Lock()
	defer p.db.mu.Unlock()
	inc := &p.db.problems[incidentID-1]
	inc.Notified, inc.NotifiedAt = !at.IsZero(), at
	return nil
}
