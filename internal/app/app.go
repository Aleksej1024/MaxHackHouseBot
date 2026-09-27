// Package app собирает сценарии бота из адаптеров
//  Используется в cmd/bot с реальными Postgres, Redis и MAX
// и в сквозных тестах с in-memory адаптерами — проверяется та же сборка,
// что работает в проде.
package app

import (
	"context"
	"fmt"
	"log/slog"

	"maxhouse/internal/adapter/maxbot"
	"maxhouse/internal/config"
	"maxhouse/internal/domain/problem"
	"maxhouse/internal/domain/rating"
	"maxhouse/internal/domain/request"
	"maxhouse/internal/domain/requesttype"
	"maxhouse/internal/usecase"
	"maxhouse/internal/usecase/chatsync"
	"maxhouse/internal/usecase/jobs"
	"maxhouse/internal/usecase/notify"
	"maxhouse/internal/usecase/onboarding"
	"maxhouse/internal/usecase/problems"
	"maxhouse/internal/usecase/registration"
	"maxhouse/internal/usecase/requests"
	"maxhouse/internal/usecase/settings"
	"maxhouse/internal/usecase/voting"
)

// Memberships — все порты, которые реализует хранилище участников.
type Memberships interface {
	usecase.MembershipRepo
	usecase.MemberDirectory
	usecase.MembershipReader
	usecase.MemberSettings
	usecase.DMAvailability
	usecase.ChatAdmins
}

// Requests — все порты, которые реализует хранилище заявок.
type Requests interface {
	usecase.RequestRepo
	usecase.RequestLocker
	usecase.RequestLifecycleRepo
	usecase.CardReader
	usecase.RequestBrowser
}

// Ratings — изменения рейтинга и периодический сброс.
type Ratings interface {
	usecase.RatingRepo
	usecase.RatingResetRepo
}

// Messenger — исходящие вызовы MAX: транспорт, публикация в чат, личка.
type Messenger interface {
	maxbot.Outbox
	usecase.ChatPublisher
	usecase.UserNotifier
	usecase.ProblemAlerter
}

// Adapters — реализации портов.
type Adapters struct {
	Chats       usecase.ChatRepo
	Memberships Memberships
	Members     usecase.MemberLister
	Requests    Requests
	Votes       usecase.VoteRepo
	Ratings     Ratings
	JobRuns     usecase.JobRuns
	Comments    usecase.CommentRepo
	Broadcasts  usecase.BroadcastRepo
	Problems    usecase.ProblemRepo
	FSM         usecase.FSMStore
	Tx          usecase.TxManager
	Clock       usecase.Clock
	Messenger   Messenger
}

// App — собранный бот: роутер апдейтов и задачи планировщика.
type App struct {
	Router      *maxbot.Router
	Lifecycle   *jobs.Lifecycle
	RatingReset *jobs.RatingReset
}

// Build собирает сценарии по конфигу.
func Build(cfg config.Config, a Adapters, log *slog.Logger) (*App, error) {
	policy, err := RatingPolicy(cfg)
	if err != nil {
		return nil, err
	}

	onb := onboarding.New(a.Memberships, a.FSM, cfg.FSM.SessionTTL, log)
	dialogs := requests.NewDialogs(a.FSM, log)
	reg := registration.New(a.Chats, a.Memberships, a.Members, userSessions{onb, dialogs}, a.Tx, cfg.Registration.AutoActivate, log)
	syncer := chatsync.New(a.Requests, a.Messenger, a.Requests, log)
	notifier := notify.New(a.Requests, a.Memberships, a.Memberships, a.Messenger, log)
	rater := voting.NewRater(a.Requests, a.Votes, a.Ratings, policy, log)
	textPolicy := request.CreatePolicy{
		MaxBodyLen:     cfg.Requests.BodyMaxLen,
		MaxAttachments: cfg.Requests.MaxAttachments,
	}

	creation := requests.NewCreation(requests.Deps{
		Chats:     onb,
		Members:   a.Memberships,
		Requests:  a.Requests,
		Publisher: a.Messenger,
		Forward:   notifier,
		Store:     a.FSM,
		Tx:        a.Tx,
		Clock:     a.Clock,
		Types:     requesttype.Default(),
		Log:       log,
	}, requests.Config{
		Policy: request.CreatePolicy{
			DailyLimit:     cfg.Requests.DailyLimit,
			MinRating:      cfg.Rating.MinToCreate,
			MaxBodyLen:     cfg.Requests.BodyMaxLen,
			MaxAttachments: cfg.Requests.MaxAttachments,
		},
		FSMTTL:         cfg.FSM.TTL,
		VotingDuration: cfg.Requests.VotingDuration,
		TTLDefault:     cfg.Requests.TTLDefault,
	})
	votingSvc := voting.New(voting.Deps{
		Requests: a.Requests,
		Members:  a.Memberships,
		Votes:    a.Votes,
		Sync:     syncer,
		Tx:       a.Tx,
		Clock:    a.Clock,
		Log:      log,
	})
	management := requests.NewManagement(requests.ManageDeps{
		Chats:     onb,
		Locker:    a.Requests,
		Lifecycle: a.Requests,
		Cards:     a.Requests,
		Rater:     rater,
		Sync:      syncer,
		Store:     a.FSM,
		Tx:        a.Tx,
		Clock:     a.Clock,
		Log:       log,
	}, requests.ManageConfig{
		Policy:    textPolicy,
		FSMTTL:    cfg.FSM.TTL,
		ListLimit: cfg.Requests.ListPageSize,
	})
	materials := requests.NewMaterials(requests.MaterialDeps{
		Chats:    onb,
		Browser:  a.Requests,
		Cards:    a.Requests,
		Members:  a.Memberships,
		Locker:   a.Requests,
		Comments: a.Comments,
		Sync:     syncer,
		Notify:   notifier,
		Store:    a.FSM,
		Tx:       a.Tx,
		Clock:    a.Clock,
		Log:      log,
	}, requests.MaterialConfig{
		Policy:   textPolicy,
		FSMTTL:   cfg.FSM.TTL,
		PageSize: cfg.Requests.ListPageSize,
	})
	broadcasts := requests.NewBroadcasts(requests.BroadcastDeps{
		Cards:      a.Requests,
		Locker:     a.Requests,
		Broadcasts: a.Broadcasts,
		Out:        a.Messenger,
		Deliver:    notifier,
		Store:      a.FSM,
		Tx:         a.Tx,
		Clock:      a.Clock,
		Log:        log,
	}, requests.BroadcastConfig{
		Policy:   textPolicy,
		FSMTTL:   cfg.FSM.TTL,
		Cooldown: cfg.Broadcast.Cooldown,
	})

	router := maxbot.NewRouter(maxbot.Scenarios{
		Registration: reg,
		Onboarding:   onb,
		Creation:     creation,
		Management:   management,
		Voting:       votingSvc,
		Dialogs:      dialogs,
		Materials:    materials,
		Broadcasts:   broadcasts,
		Settings:     settings.New(onb, a.Memberships, cfg.Rating.MinToCreate),
		Problems: problems.New(problems.Deps{
			Chats:   onb,
			Repo:    a.Problems,
			Members: a.Members,
			Admins:  a.Memberships,
			Alerter: a.Messenger,
			Tx:      a.Tx,
			Clock:   a.Clock,
			Log:     log,
		}, problem.Policy{Threshold: cfg.Problems.AlertThreshold, Window: cfg.Problems.AlertWindow}),
	}, a.Messenger, log, maxbot.WithLocation(cfg.Display.Location))

	return &App{
		Router: router,
		Lifecycle: jobs.NewLifecycle(jobs.LifecycleDeps{
			Requests: a.Requests,
			Rater:    rater,
			Sync:     syncer,
			Tx:       a.Tx,
			Clock:    a.Clock,
			Log:      log,
		}, cfg.Scheduler.BatchSize),
		RatingReset: jobs.NewRatingReset(a.JobRuns, a.Ratings, cfg.Rating.ResetInterval, a.Tx, a.Clock, log),
	}, nil
}

// RatingPolicy — параметры рейтинга из конфига.
func RatingPolicy(cfg config.Config) (rating.Policy, error) {
	appliesTo, err := rating.ParseAppliesTo(cfg.Rating.AppliesToTypes)
	if err != nil {
		return rating.Policy{}, fmt.Errorf("RATING_APPLIES_TO_TYPES: %w", err)
	}
	return rating.Policy{
		MinVotes:       cfg.Rating.MinVotes,
		ConfirmShare:   cfg.Rating.ConfirmShare,
		RefuteShare:    cfg.Rating.RefuteShare,
		DeltaConfirmed: cfg.Rating.DeltaConfirmed,
		DeltaRefuted:   cfg.Rating.DeltaRefuted,
		AppliesTo:      appliesTo,
	}, nil
}

// userSessions — сессия и диалог в личке для регистрации: при выходе из
// выбранного дома сессия переходит на оставшийся дом (или сбрасывается),
// а незавершённый диалог по ушедшему дому прерывается.
type userSessions struct {
	onb     *onboarding.Service
	dialogs *requests.Dialogs
}

func (s userSessions) LeftChat(ctx context.Context, userID, chatID int64) error {
	switched, err := s.onb.LeftChat(ctx, userID, chatID)
	if err != nil || !switched {
		return err
	}
	return s.dialogs.Reset(ctx, userID)
}
