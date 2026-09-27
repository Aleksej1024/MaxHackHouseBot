//go:build integration

// Интеграционные тесты репозиториев на реальном Postgres.
// Запуск: make test-integration (нужен Docker). Контейнер поднимается один
// раз на пакет, схема — из migrations/*.up.sql, перед каждым тестом база
// очищается (resetDB), поэтому тесты не параллелятся.
package postgres

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"maxhouse/internal/domain/housechat"
	"maxhouse/internal/domain/membership"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	os.Exit(runWithPostgres(m))
}

func runWithPostgres(m *testing.M) int {
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("bot"),
		tcpostgres.WithUsername("bot"),
		tcpostgres.WithPassword("bot"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "запуск контейнера Postgres:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "строка подключения:", err)
		return 1
	}
	testPool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "пул pgx:", err)
		return 1
	}
	defer testPool.Close()

	if err := applyMigrations(ctx, "up"); err != nil {
		fmt.Fprintln(os.Stderr, "миграции:", err)
		return 1
	}
	return m.Run()
}

// migrationFiles — файлы миграций направления (up по возрастанию, down по убыванию).
func migrationFiles(direction string) ([]string, error) {
	_, self, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(self), "..", "..", "..", "migrations")
	files, err := filepath.Glob(filepath.Join(dir, "*."+direction+".sql"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("нет миграций %s в %s", direction, dir)
	}
	sort.Strings(files)
	if direction == "down" {
		for i, j := 0, len(files)-1; i < j; i, j = i+1, j-1 {
			files[i], files[j] = files[j], files[i]
		}
	}
	return files, nil
}

func applyMigrations(ctx context.Context, direction string) error {
	files, err := migrationFiles(direction)
	if err != nil {
		return err
	}
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := testPool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

// allTables — все таблицы схемы; resetDB проверяет, что список не отстал от миграций.
var allTables = []string{
	"house_chats", "users", "memberships", "requests", "comments", "attachments",
	"votes", "rating_events", "scheduler_runs", "broadcasts", "problem_reports", "problem_report_users",
}

// resetDB очищает все таблицы и сбрасывает последовательности id.
func resetDB(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var n int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`,
	).Scan(&n))
	require.Equal(t, len(allTables), n, "в allTables учтены не все таблицы схемы")
	_, err := testPool.Exec(ctx, "TRUNCATE "+strings.Join(allTables, ", ")+" RESTART IDENTITY CASCADE")
	require.NoError(t, err)
}

// --- Хелперы заполнения данных ---

var t0 = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func seedChat(t *testing.T, id int64, status housechat.Status) {
	t.Helper()
	require.NoError(t, NewChatRepo(testPool).Upsert(context.Background(),
		housechat.Chat{ID: id, Title: fmt.Sprintf("Дом %d", id), Status: status}))
}

func seedUser(t *testing.T, id int64, nickname string) {
	t.Helper()
	require.NoError(t, NewMembershipRepo(testPool).UpsertUser(context.Background(),
		membership.User{ID: id, Nickname: nickname}))
}

func seedMember(t *testing.T, chatID, userID int64) {
	t.Helper()
	seedUserIfMissing(t, userID)
	require.NoError(t, NewMembershipRepo(testPool).UpsertMembership(context.Background(), membership.Membership{
		ChatID: chatID, UserID: userID, Role: membership.RoleMember, Status: membership.StatusActive,
	}))
}

func seedUserIfMissing(t *testing.T, id int64) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO users (id, nickname) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, fmt.Sprintf("user%d", id))
	require.NoError(t, err)
}

// seedRequest создаёт заявку; нулевые поля заполняются разумными значениями.
func seedRequest(t *testing.T, r request.Request, atts ...request.Attachment) int64 {
	t.Helper()
	if r.Type == "" {
		r.Type = requesttype.Complaint
	}
	if r.Status == "" {
		r.Status = request.StatusOpen
	}
	if r.Body == "" {
		r.Body = "текст заявки"
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = t0
	}
	if r.VotingEndsAt.IsZero() {
		r.VotingEndsAt = r.CreatedAt.Add(72 * time.Hour)
	}
	if r.ExpiresAt.IsZero() {
		r.ExpiresAt = r.CreatedAt.Add(14 * 24 * time.Hour)
	}
	var id int64
	err := NewTxManager(testPool).Do(context.Background(), func(ctx context.Context) error {
		var err error
		id, err = NewRequestRepo(testPool).Create(ctx, r, atts)
		return err
	})
	require.NoError(t, err)
	return id
}

func setRequestStatus(t *testing.T, id int64, status request.Status) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), `UPDATE requests SET status = $2 WHERE id = $1`, id, string(status))
	require.NoError(t, err)
}

// --- Миграции ---

func TestMigrations_DownAndUpAgain(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, applyMigrations(ctx, "down"), "все down применяются по убыванию")

	var n int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).Scan(&n))
	require.Zero(t, n, "после down схема пуста")

	require.NoError(t, applyMigrations(ctx, "up"), "up применяется повторно")
	resetDB(t)
}
