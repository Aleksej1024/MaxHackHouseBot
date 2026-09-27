// Бот домовых чатов для MAX.
//
// Бинарник только запускает бота: HTTP-сервер (/healthz, /readyz и, в режиме
// webhook, /webhook), приём обновлений MAX и планировщик. Способ приёма
// обновлений задаётся конфигом (UPDATES_MODE), а не аргументами:
//   - webhook — при старте бот сам подписывается на WEBHOOK_URL;
//   - longpoll — при старте бот снимает webhook-подписки и опрашивает MAX.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	maxbotapi "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/redis/go-redis/v9"

	"maxhouse/internal/adapter/httpserver"
	"maxhouse/internal/adapter/maxbot"
	"maxhouse/internal/adapter/maxbot/intake"
	"maxhouse/internal/adapter/maxbot/longpoll"
	"maxhouse/internal/adapter/maxbot/webhook"
	"maxhouse/internal/adapter/postgres"
	"maxhouse/internal/adapter/redisfsm"
	"maxhouse/internal/adapter/sysclock"
	"maxhouse/internal/app"
	"maxhouse/internal/config"
	"maxhouse/internal/logging"
	"maxhouse/internal/scheduler"
	"maxhouse/internal/usecase/jobs"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run собирает и запускает бота до сигнала остановки. Ошибка — если бот
// не смог стартовать или остановился из-за сбоя (подписка, HTTP-сервер);
// все defer к этому моменту отрабатывают.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logging.New(cfg.Env, cfg.Log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	pool, err := pgxpool.New(ctx, cfg.Postgres.DSN)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr})
	defer func() { _ = rdb.Close() }()

	api, err := newAPI(cfg)
	if err != nil {
		return fmt.Errorf("клиент MAX: %w", err)
	}

	bot, err := app.Build(cfg, app.Adapters{
		Chats:       postgres.NewChatRepo(pool),
		Memberships: postgres.NewMembershipRepo(pool),
		Members:     maxbot.NewMemberLister(api, retryOptions(cfg)),
		Requests:    postgres.NewRequestRepo(pool),
		Votes:       postgres.NewVoteRepo(pool),
		Ratings:     postgres.NewRatingRepo(pool),
		JobRuns:     postgres.NewJobRuns(pool),
		Comments:    postgres.NewCommentRepo(pool),
		Broadcasts:  postgres.NewBroadcastRepo(pool),
		Problems:    postgres.NewProblemRepo(pool),
		FSM:         redisfsm.New(rdb),
		Tx:          postgres.NewTxManager(pool),
		Clock:       sysclock.Clock{},
		Messenger:   maxbot.NewMessenger(api, retryOptions(cfg)),
	}, log)
	if err != nil {
		return err
	}

	// Подсказки команд в клиенте MAX. Не критично: команды разбирает роутер
	// и без этого, поэтому ошибка — только предупреждение.
	cmdCtx, cancelCmd := context.WithTimeout(ctx, cfg.MaxBot.CallTimeout)
	if err := maxbot.RegisterCommands(cmdCtx, api.Bots); err != nil {
		log.Warn("список команд в MAX не обновлён", "error", err)
	}
	cancelCmd()

	// Приём обновлений: общий антидубль и очередь для webhook и long polling.
	updates := intake.New(intake.Options{
		Processor: bot.Router,
		Dedup:     intake.NewRedisDedupStore(rdb),
		DedupTTL:  cfg.Dedup.TTL,
		Log:       log,
	})
	updates.Start()

	// HTTP-сервер работает в обоих режимах: /healthz и /readyz нужны
	// health-check контейнера. /webhook добавляется только в режиме webhook.
	mux := http.NewServeMux()
	httpserver.RegisterHealth(mux, pool, httpserver.NewRedisPinger(rdb), log)

	// Источники обновлений останавливаются раньше очереди:
	// intake.Shutdown дожидается обработки уже принятых апдейтов.
	var sources sync.WaitGroup
	startSource := func(name string, run func(ctx context.Context) error) {
		sources.Add(1)
		go func() {
			defer sources.Done()
			if err := run(ctx); err != nil {
				cancel(fmt.Errorf("%s: %w", name, err))
			}
		}()
	}
	startHTTP := func() {
		srv := httpserver.New(httpserver.Options{
			Addr:    cfg.HTTP.Addr,
			Handler: mux,
			// Бюджет остановки общий, HTTP укладывается в него же.
			ShutdownTimeout: cfg.HTTP.ShutdownTimeout,
			Log:             log,
		})
		log.Info("HTTP-сервер запущен", "addr", cfg.HTTP.Addr, "updates_mode", string(cfg.Webhook.Mode))
		startSource("HTTP-сервер", srv.Run)
	}

	switch cfg.Webhook.Mode {
	case config.UpdatesWebhook:
		mux.Handle("/webhook", webhook.Handler(api, cfg.Webhook.Secret, updates))
		startHTTP()
		// Сервер уже слушает: MAX может сразу присылать обновления.
		subCtx, cancelSub := context.WithTimeout(ctx, cfg.MaxBot.CallTimeout)
		err := webhook.EnsureSubscription(subCtx, api.Subscriptions, cfg.Webhook.URL, cfg.Webhook.Secret, log)
		cancelSub()
		if err != nil {
			cancel(err)
		}
	case config.UpdatesLongPoll:
		startHTTP()
		startSource("long polling", longpoll.New(longpoll.Options{
			Source:      api.Subscriptions,
			Intake:      updates,
			BaseDelay:   cfg.MaxBot.RetryBaseDelay,
			MaxDelay:    cfg.MaxBot.RetryMaxDelay,
			CallTimeout: cfg.MaxBot.CallTimeout,
			Log:         log,
		}).Run)
	}

	sched := scheduler.New(cfg.Scheduler.Interval, log,
		scheduler.Job{Name: "request_lifecycle", Run: bot.Lifecycle.Run},
		scheduler.Job{Name: jobs.RatingResetJob, Run: bot.RatingReset.Run},
	)
	var background sync.WaitGroup
	background.Add(1)
	go func() {
		defer background.Done()
		_ = sched.Run(ctx)
	}()

	// Остановка (сигнал или сбой): всё — в пределах SHUTDOWN_TIMEOUT с этого
	// момента. Docker ждёт stop_grace_period, который должен быть больше.
	<-ctx.Done()
	failure := context.Cause(ctx)
	if errors.Is(failure, context.Canceled) {
		failure = nil // сигнал остановки — не ошибка
	}
	if failure != nil {
		log.Error("бот останавливается из-за ошибки", "error", failure)
	}
	log.Info("останавливаем бота", "timeout", cfg.HTTP.ShutdownTimeout)
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancelShutdown()

	sources.Wait()
	updates.Shutdown(shutdownCtx)
	background.Wait()
	log.Info("бот остановлен")
	return failure
}

// newAPI создаёт клиент MAX. Таймаут HTTP-клиента больше таймаута long
// polling: иначе запрос /updates обрывался бы раньше ответа сервера. Остальные
// вызовы ограничены MAX_CALL_TIMEOUT через контекст (раздел 13).
func newAPI(cfg config.Config) (*maxbotapi.Api, error) {
	return maxbotapi.NewApi(cfg.MaxBot.Token,
		maxbotapi.WithPollingTimeout(cfg.Webhook.LongPollTimeout),
		maxbotapi.WithHTTPClient(&http.Client{Timeout: cfg.Webhook.LongPollTimeout + 15*time.Second}),
	)
}

func retryOptions(cfg config.Config) maxbot.RetryOptions {
	return maxbot.RetryOptions{
		Attempts:    cfg.MaxBot.RetryAttempts,
		BaseDelay:   cfg.MaxBot.RetryBaseDelay,
		MaxDelay:    cfg.MaxBot.RetryMaxDelay,
		CallTimeout: cfg.MaxBot.CallTimeout,
	}
}
