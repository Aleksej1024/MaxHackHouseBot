// Package config загружает конфигурацию приложения из переменных окружения.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // база часовых поясов: в минимальном образе её нет

	"maxhouse/internal/domain/rating"
)

type Config struct {
	Env          string
	Log          Log
	HTTP         HTTP
	MaxBot       MaxBot
	Postgres     Postgres
	Redis        Redis
	Webhook      Webhook
	Dedup        Dedup
	FSM          FSM
	Requests     Requests
	Scheduler    Scheduler
	Broadcast    Broadcast
	Rating       Rating
	Registration Registration
	Display      Display
	Problems     Problems
}

type Log struct {
	Level      slog.Level
	FilePath   string
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
}

type HTTP struct {
	Addr string
	// ShutdownTimeout — общий бюджет на остановку бота с момента сигнала:
	// завершение HTTP-запросов, дообработка очереди (SHUTDOWN_TIMEOUT).
	ShutdownTimeout time.Duration
}

// Display — параметры отображения для пользователей.
type Display struct {
	// Location — часовой пояс времени в сообщениях (DISPLAY_TIMEZONE).
	Location *time.Location
}

type MaxBot struct {
	Token          string
	RetryAttempts  int
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
	CallTimeout    time.Duration
}

type Postgres struct {
	DSN string
}

type Redis struct {
	Addr string
}

// UpdatesMode — способ получения обновлений MAX (UPDATES_MODE).
type UpdatesMode string

const (
	// UpdatesWebhook — MAX присылает обновления на HTTPS-адрес бота.
	UpdatesWebhook UpdatesMode = "webhook"
	// UpdatesLongPoll — бот сам опрашивает MAX (GET /updates). Нужен, пока
	// для webhook нет подходящего сертификата.
	UpdatesLongPoll UpdatesMode = "longpoll"
)

type Webhook struct {
	Mode   UpdatesMode
	URL    string
	Secret string
	// LongPollTimeout — сколько MAX держит запрос /updates без новых
	// обновлений (LONGPOLL_TIMEOUT, от 1s до 90s).
	LongPollTimeout time.Duration
}

type Dedup struct {
	TTL time.Duration
}

type FSM struct {
	TTL time.Duration
	// SessionTTL — срок жизни сессии в личке: активный чат и флаг
	// показанного приветствия.
	SessionTTL time.Duration
}

type Requests struct {
	DailyLimit     int
	VotingDuration time.Duration
	TTLDefault     time.Duration
	// BodyMaxLen — максимум символов в тексте заявки.
	BodyMaxLen int
	// MaxAttachments — максимум вложений в заявке.
	MaxAttachments int
	// ListPageSize — сколько заявок показывать в списках в личке.
	ListPageSize int
}

type Scheduler struct {
	Interval time.Duration
	// BatchSize — сколько заявок одна задача обрабатывает за тик.
	BatchSize int
}

type Broadcast struct {
	Cooldown time.Duration
}

type Rating struct {
	MinVotes       int
	ConfirmShare   float64
	RefuteShare    float64
	DeltaConfirmed int
	DeltaRefuted   int
	MinToCreate    int
	AppliesToTypes string
	// ResetInterval — как часто обнулять отрицательные рейтинги.
	ResetInterval time.Duration
}

// Problems — «Сообщить о проблеме»: оповещение в чат,
// когда за AlertWindow о проблеме сообщили AlertThreshold разных жителей.
type Problems struct {
	AlertThreshold int
	AlertWindow    time.Duration
}

type Registration struct {
	// AutoActivate: true — новый чат сразу active, false — pending
	// до подтверждения организатора.
	AutoActivate bool
}

// webhookSecretRe — допустимый секрет webhook по схеме MAX.
var webhookSecretRe = regexp.MustCompile(`^[A-Za-z0-9_-]{5,256}$`)

// Load читает переменные окружения и возвращает конфигурацию.
// Отсутствие обязательных переменных и противоречивые значения — ошибка.
func Load() (Config, error) {
	var cfg Config
	var missing []string

	cfg.Env = envString("APP_ENV", "dev")
	cfg.MaxBot.Token = required("MAX_BOT_TOKEN", &missing)
	cfg.Postgres.DSN = required("POSTGRES_DSN", &missing)
	cfg.Redis.Addr = required("REDIS_ADDR", &missing)
	switch mode := UpdatesMode(strings.ToLower(envString("UPDATES_MODE", string(UpdatesWebhook)))); mode {
	case UpdatesWebhook:
		cfg.Webhook.Mode = mode
		cfg.Webhook.URL = required("WEBHOOK_URL", &missing)
		cfg.Webhook.Secret = required("WEBHOOK_SECRET", &missing)
	case UpdatesLongPoll:
		cfg.Webhook.Mode = mode
	default:
		return Config{}, fmt.Errorf("переменная UPDATES_MODE: ожидается webhook или longpoll, получено %q", mode)
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("не заданы обязательные переменные окружения: %s", strings.Join(missing, ", "))
	}
	if cfg.Webhook.Mode == UpdatesWebhook {
		if !strings.HasPrefix(cfg.Webhook.URL, "https://") {
			return Config{}, fmt.Errorf("WEBHOOK_URL: MAX принимает только https-адрес")
		}
		// Формат из схемы MAX Bot API; nginx в prod сверяет секрет как есть.
		if !webhookSecretRe.MatchString(cfg.Webhook.Secret) {
			return Config{}, fmt.Errorf("WEBHOOK_SECRET: 5–256 символов, только латиница, цифры, _ и - (например, openssl rand -hex 32)")
		}
	}

	defaultLogLevel := slog.LevelDebug
	if cfg.Env == "prod" {
		defaultLogLevel = slog.LevelInfo
	}

	var err error
	if cfg.Log.Level, err = envLogLevel("LOG_LEVEL", defaultLogLevel); err != nil {
		return Config{}, err
	}
	cfg.Log.FilePath = envString("LOG_FILE_PATH", "/var/log/bot/bot.log")
	if cfg.Log.MaxSizeMB, err = envInt("LOG_MAX_SIZE_MB", 50); err != nil {
		return Config{}, err
	}
	if cfg.Log.MaxBackups, err = envInt("LOG_MAX_BACKUPS", 7); err != nil {
		return Config{}, err
	}
	if cfg.Log.MaxAgeDays, err = envInt("LOG_MAX_AGE_DAYS", 14); err != nil {
		return Config{}, err
	}

	cfg.HTTP.Addr = envString("HTTP_ADDR", ":8080")
	if cfg.HTTP.ShutdownTimeout, err = envDuration("SHUTDOWN_TIMEOUT", 20*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.HTTP.ShutdownTimeout <= 0 {
		return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT должен быть больше нуля")
	}

	tz := envString("DISPLAY_TIMEZONE", "Europe/Moscow")
	if cfg.Display.Location, err = time.LoadLocation(tz); err != nil {
		return Config{}, fmt.Errorf("переменная DISPLAY_TIMEZONE: неизвестный часовой пояс %q", tz)
	}

	if cfg.Dedup.TTL, err = envDuration("UPDATE_DEDUP_TTL", 24*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.Webhook.LongPollTimeout, err = envDuration("LONGPOLL_TIMEOUT", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.Webhook.LongPollTimeout < time.Second || cfg.Webhook.LongPollTimeout > 90*time.Second {
		return Config{}, fmt.Errorf("LONGPOLL_TIMEOUT (%s) должен быть от 1s до 90s", cfg.Webhook.LongPollTimeout)
	}

	if cfg.MaxBot.RetryAttempts, err = envInt("MAX_RETRY_ATTEMPTS", 3); err != nil {
		return Config{}, err
	}
	if cfg.MaxBot.RetryBaseDelay, err = envDuration("MAX_RETRY_BASE_DELAY", 500*time.Millisecond); err != nil {
		return Config{}, err
	}
	if cfg.MaxBot.RetryMaxDelay, err = envDuration("MAX_RETRY_MAX_DELAY", 5*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.MaxBot.CallTimeout, err = envDuration("MAX_CALL_TIMEOUT", 5*time.Second); err != nil {
		return Config{}, err
	}

	if cfg.FSM.TTL, err = envDuration("FSM_TTL", 30*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.FSM.SessionTTL, err = envDuration("SESSION_TTL", 24*time.Hour); err != nil {
		return Config{}, err
	}

	if cfg.Requests.DailyLimit, err = envInt("DAILY_REQUEST_LIMIT", 3); err != nil {
		return Config{}, err
	}
	if cfg.Requests.VotingDuration, err = envDuration("VOTING_DURATION", 72*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.Requests.TTLDefault, err = envDuration("REQUEST_TTL_DEFAULT", 14*24*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.Requests.BodyMaxLen, err = envInt("REQUEST_BODY_MAX_LEN", 3000); err != nil {
		return Config{}, err
	}
	if cfg.Requests.MaxAttachments, err = envInt("REQUEST_MAX_ATTACHMENTS", 10); err != nil {
		return Config{}, err
	}
	if cfg.Requests.VotingDuration >= cfg.Requests.TTLDefault {
		return Config{}, fmt.Errorf("VOTING_DURATION (%s) должна быть меньше REQUEST_TTL_DEFAULT (%s)",
			cfg.Requests.VotingDuration, cfg.Requests.TTLDefault)
	}

	if cfg.Scheduler.Interval, err = envDuration("SCHEDULER_INTERVAL", time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.Scheduler.BatchSize, err = envInt("SCHEDULER_BATCH_SIZE", 100); err != nil {
		return Config{}, err
	}
	if cfg.Requests.ListPageSize, err = envInt("LIST_PAGE_SIZE", 10); err != nil {
		return Config{}, err
	}
	if cfg.Broadcast.Cooldown, err = envDuration("BROADCAST_COOLDOWN", 24*time.Hour); err != nil {
		return Config{}, err
	}

	if cfg.Rating.MinVotes, err = envInt("RATING_MIN_VOTES", 1); err != nil {
		return Config{}, err
	}
	if cfg.Rating.ConfirmShare, err = envFloat("RATING_CONFIRM_SHARE", 0.5); err != nil {
		return Config{}, err
	}
	if cfg.Rating.RefuteShare, err = envFloat("RATING_REFUTE_SHARE", 0.5); err != nil {
		return Config{}, err
	}
	if cfg.Rating.DeltaConfirmed, err = envInt("RATING_DELTA_CONFIRMED", 1); err != nil {
		return Config{}, err
	}
	if cfg.Rating.DeltaRefuted, err = envInt("RATING_DELTA_REFUTED", 1); err != nil {
		return Config{}, err
	}
	if cfg.Rating.MinToCreate, err = envInt("RATING_MIN_TO_CREATE", -3); err != nil {
		return Config{}, err
	}
	cfg.Rating.AppliesToTypes = envString("RATING_APPLIES_TO_TYPES", "all")
	if _, err := rating.ParseAppliesTo(cfg.Rating.AppliesToTypes); err != nil {
		return Config{}, fmt.Errorf("переменная RATING_APPLIES_TO_TYPES: %w", err)
	}
	if cfg.Rating.ResetInterval, err = envDuration("RATING_RESET_INTERVAL", 7*24*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.Rating.ResetInterval <= 0 {
		return Config{}, fmt.Errorf("RATING_RESET_INTERVAL должен быть больше нуля")
	}

	if cfg.Registration.AutoActivate, err = envBool("AUTO_CHAT_REGISTRATION", true); err != nil {
		return Config{}, err
	}

	if cfg.Problems.AlertThreshold, err = envInt("PROBLEM_ALERT_THRESHOLD", 3); err != nil {
		return Config{}, err
	}
	if cfg.Problems.AlertThreshold < 1 {
		return Config{}, fmt.Errorf("PROBLEM_ALERT_THRESHOLD должен быть не меньше 1")
	}
	if cfg.Problems.AlertWindow, err = envDuration("PROBLEM_ALERT_WINDOW", 30*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.Problems.AlertWindow <= 0 {
		return Config{}, fmt.Errorf("PROBLEM_ALERT_WINDOW должен быть больше нуля")
	}

	return cfg, nil
}

func required(name string, missing *[]string) string {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		*missing = append(*missing, name)
		return ""
	}
	return v
}

func envString(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) (int, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("переменная %s: ожидается целое число, получено %q", name, v)
	}
	return n, nil
}

func envFloat(name string, def float64) (float64, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("переменная %s: ожидается число, получено %q", name, v)
	}
	return f, nil
}

func envBool(name string, def bool) (bool, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def, nil
	}
	switch strings.ToLower(v) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("переменная %s: ожидается true|false|1|0, получено %q", name, v)
	}
}

// envDuration разбирает длительность. Кроме формата time.ParseDuration
// поддерживается суффикс "d" для дней (например "14d").
func envDuration(name string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def, nil
	}
	d, err := parseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("переменная %s: %w", name, err)
	}
	return d, nil
}

func parseDuration(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err == nil && days >= 0 {
			return time.Duration(days * float64(24*time.Hour)), nil
		}
	}
	return 0, fmt.Errorf("ожидается длительность (например 500ms, 30m, 72h, 14d), получено %q", s)
}

func envLogLevel(name string, def slog.Level) (slog.Level, error) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return def, nil
	}
	switch strings.ToLower(v) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("переменная %s: ожидается debug|info|warn|error, получено %q", name, v)
	}
}
