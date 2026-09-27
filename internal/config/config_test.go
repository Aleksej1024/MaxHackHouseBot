package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("MAX_BOT_TOKEN", "token")
	t.Setenv("POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_ADDR", "localhost:6379")
	t.Setenv("WEBHOOK_URL", "https://example.com/webhook")
	t.Setenv("WEBHOOK_SECRET", "secret-1_A")
}

func TestLoad_RequiredMissing(t *testing.T) {
	_, err := Load()
	require.Error(t, err)
	for _, name := range []string{"MAX_BOT_TOKEN", "POSTGRES_DSN", "REDIS_ADDR", "WEBHOOK_URL", "WEBHOOK_SECRET"} {
		assert.Contains(t, err.Error(), name)
	}
}

func TestLoad_Defaults(t *testing.T) {
	setRequired(t)

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "dev", cfg.Env)
	assert.Equal(t, slog.LevelDebug, cfg.Log.Level)
	assert.Equal(t, "/var/log/bot/bot.log", cfg.Log.FilePath)
	assert.Equal(t, 50, cfg.Log.MaxSizeMB)
	assert.Equal(t, 7, cfg.Log.MaxBackups)
	assert.Equal(t, 14, cfg.Log.MaxAgeDays)
	assert.Equal(t, ":8080", cfg.HTTP.Addr)
	assert.Equal(t, 20*time.Second, cfg.HTTP.ShutdownTimeout)
	assert.Equal(t, 24*time.Hour, cfg.Dedup.TTL)
	assert.Equal(t, 3, cfg.MaxBot.RetryAttempts)
	assert.Equal(t, 500*time.Millisecond, cfg.MaxBot.RetryBaseDelay)
	assert.Equal(t, 5*time.Second, cfg.MaxBot.RetryMaxDelay)
	assert.Equal(t, 5*time.Second, cfg.MaxBot.CallTimeout)
	assert.Equal(t, 30*time.Minute, cfg.FSM.TTL)
	assert.Equal(t, 24*time.Hour, cfg.FSM.SessionTTL)
	assert.Equal(t, 3, cfg.Requests.DailyLimit)
	assert.Equal(t, 72*time.Hour, cfg.Requests.VotingDuration)
	assert.Equal(t, 14*24*time.Hour, cfg.Requests.TTLDefault)
	assert.Equal(t, 3000, cfg.Requests.BodyMaxLen)
	assert.Equal(t, 10, cfg.Requests.MaxAttachments)
	assert.Equal(t, time.Minute, cfg.Scheduler.Interval)
	assert.Equal(t, 100, cfg.Scheduler.BatchSize)
	assert.Equal(t, 10, cfg.Requests.ListPageSize)
	assert.Equal(t, "Europe/Moscow", cfg.Display.Location.String())
	assert.Equal(t, 24*time.Hour, cfg.Broadcast.Cooldown)
	assert.Equal(t, 1, cfg.Rating.MinVotes)
	assert.InDelta(t, 0.5, cfg.Rating.ConfirmShare, 1e-9)
	assert.InDelta(t, 0.5, cfg.Rating.RefuteShare, 1e-9)
	assert.Equal(t, 1, cfg.Rating.DeltaConfirmed)
	assert.Equal(t, 1, cfg.Rating.DeltaRefuted)
	assert.Equal(t, -3, cfg.Rating.MinToCreate)
	assert.Equal(t, "all", cfg.Rating.AppliesToTypes)
	assert.Equal(t, 7*24*time.Hour, cfg.Rating.ResetInterval)
	assert.Equal(t, 3, cfg.Problems.AlertThreshold)
	assert.Equal(t, 30*time.Minute, cfg.Problems.AlertWindow)
}

func TestLoad_RatingAppliesToValidated(t *testing.T) {
	setRequired(t)
	t.Setenv("RATING_APPLIES_TO_TYPES", "complaint,,poll")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RATING_APPLIES_TO_TYPES")
}

func TestLoad_RatingResetInterval(t *testing.T) {
	setRequired(t)
	for in, want := range map[string]time.Duration{"14d": 14 * 24 * time.Hour, "6m": 6 * time.Minute, "12h": 12 * time.Hour} {
		t.Setenv("RATING_RESET_INTERVAL", in)
		cfg, err := Load()
		require.NoError(t, err, in)
		assert.Equal(t, want, cfg.Rating.ResetInterval, in)
	}
	for _, bad := range []string{"weekly", "0s", "-1d"} {
		t.Setenv("RATING_RESET_INTERVAL", bad)
		_, err := Load()
		require.Error(t, err, bad)
		assert.Contains(t, err.Error(), "RATING_RESET_INTERVAL", bad)
	}
}

func TestLoad_DisplayTimezone(t *testing.T) {
	setRequired(t)
	t.Setenv("DISPLAY_TIMEZONE", "Asia/Novosibirsk")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "Asia/Novosibirsk", cfg.Display.Location.String())

	t.Setenv("DISPLAY_TIMEZONE", "Mars/Olympus")
	_, err = Load()
	require.Error(t, err)
}

func TestLoad_UpdatesMode(t *testing.T) {
	t.Run("по умолчанию webhook, URL и секрет обязательны", func(t *testing.T) {
		setRequired(t)
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, UpdatesWebhook, cfg.Webhook.Mode)
		assert.Equal(t, 30*time.Second, cfg.Webhook.LongPollTimeout)
	})

	t.Run("longpoll без WEBHOOK_URL и WEBHOOK_SECRET", func(t *testing.T) {
		t.Setenv("MAX_BOT_TOKEN", "token")
		t.Setenv("POSTGRES_DSN", "postgres://u:p@localhost:5432/db")
		t.Setenv("REDIS_ADDR", "localhost:6379")
		t.Setenv("UPDATES_MODE", "LongPoll")
		t.Setenv("LONGPOLL_TIMEOUT", "45s")
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, UpdatesLongPoll, cfg.Webhook.Mode)
		assert.Equal(t, 45*time.Second, cfg.Webhook.LongPollTimeout)
	})

	t.Run("webhook: секрет в формате MAX и https-адрес", func(t *testing.T) {
		for _, bad := range []string{"abcd", "с пробелом", "pa$$word", "a.b.c.d.e", strings.Repeat("a", 257)} {
			setRequired(t)
			t.Setenv("WEBHOOK_SECRET", bad)
			_, err := Load()
			require.Error(t, err, bad)
			assert.Contains(t, err.Error(), "WEBHOOK_SECRET")
		}
		setRequired(t)
		t.Setenv("WEBHOOK_URL", "http://example.com/webhook")
		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "WEBHOOK_URL")
	})

	t.Run("неизвестный режим", func(t *testing.T) {
		setRequired(t)
		t.Setenv("UPDATES_MODE", "polling")
		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "UPDATES_MODE")
	})

	t.Run("таймаут вне границ MAX", func(t *testing.T) {
		setRequired(t)
		t.Setenv("LONGPOLL_TIMEOUT", "2m")
		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "LONGPOLL_TIMEOUT")
	})
}

func TestLoad_ShutdownTimeoutPositive(t *testing.T) {
	setRequired(t)
	t.Setenv("SHUTDOWN_TIMEOUT", "0s")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SHUTDOWN_TIMEOUT")
}

func TestLoad_ProdDefaultLogLevel(t *testing.T) {
	setRequired(t)
	t.Setenv("APP_ENV", "prod")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, slog.LevelInfo, cfg.Log.Level)
}

func TestLoad_ParseValues(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		check func(t *testing.T, cfg Config)
	}{
		{
			name:  "уровень логов",
			env:   map[string]string{"LOG_LEVEL": "warn"},
			check: func(t *testing.T, cfg Config) { assert.Equal(t, slog.LevelWarn, cfg.Log.Level) },
		},
		{
			name:  "длительность в днях",
			env:   map[string]string{"REQUEST_TTL_DEFAULT": "30d", "VOTING_DURATION": "24h"},
			check: func(t *testing.T, cfg Config) { assert.Equal(t, 30*24*time.Hour, cfg.Requests.TTLDefault) },
		},
		{
			name:  "длительность go",
			env:   map[string]string{"FSM_TTL": "45m"},
			check: func(t *testing.T, cfg Config) { assert.Equal(t, 45*time.Minute, cfg.FSM.TTL) },
		},
		{
			name: "числа и доли рейтинга",
			env:  map[string]string{"RATING_MIN_TO_CREATE": "-5", "RATING_CONFIRM_SHARE": "0.75"},
			check: func(t *testing.T, cfg Config) {
				assert.Equal(t, -5, cfg.Rating.MinToCreate)
				assert.InDelta(t, 0.75, cfg.Rating.ConfirmShare, 1e-9)
			},
		},
		{
			name:  "список типов рейтинга",
			env:   map[string]string{"RATING_APPLIES_TO_TYPES": "complaint,initiative"},
			check: func(t *testing.T, cfg Config) { assert.Equal(t, "complaint,initiative", cfg.Rating.AppliesToTypes) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequired(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg, err := Load()
			require.NoError(t, err)
			tt.check(t, cfg)
		})
	}
}

func TestLoad_InvalidValues(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "нечисловой int",
			env:  map[string]string{"DAILY_REQUEST_LIMIT": "три"},
			want: "DAILY_REQUEST_LIMIT",
		},
		{
			name: "нечисловая длительность",
			env:  map[string]string{"FSM_TTL": "полчаса"},
			want: "FSM_TTL",
		},
		{
			name: "неизвестный уровень логов",
			env:  map[string]string{"LOG_LEVEL": "verbose"},
			want: "LOG_LEVEL",
		},
		{
			name: "порог оповещения о проблеме меньше 1",
			env:  map[string]string{"PROBLEM_ALERT_THRESHOLD": "0"},
			want: "PROBLEM_ALERT_THRESHOLD",
		},
		{
			name: "пустое окно оповещения о проблеме",
			env:  map[string]string{"PROBLEM_ALERT_WINDOW": "0s"},
			want: "PROBLEM_ALERT_WINDOW",
		},
		{
			name: "голосование длиннее срока жизни",
			env:  map[string]string{"VOTING_DURATION": "72h", "REQUEST_TTL_DEFAULT": "24h"},
			want: "VOTING_DURATION",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequired(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}
