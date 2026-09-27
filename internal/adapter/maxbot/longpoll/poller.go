// Package longpoll — приём обновлений MAX через long polling (GET /updates),
// альтернатива webhook, включается UPDATES_MODE=longpoll.
//
// MAX отдаёт обновления через /updates, только пока у бота нет
// webhook-подписки, поэтому при старте подписки снимаются. Подтверждённый
// marker хранит сервер: переданный marker подтверждает всё до него, после
// перезапуска придут неподтверждённые обновления, повторы отсекает антидубль.
package longpoll

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

// Source — методы подписок MAX API, нужные опросу.
type Source interface {
	GetUpdates(ctx context.Context, marker int64) ([]model.Update, int64, error)
	GetSubscriptions(ctx context.Context) (model.GetSubscriptionsResult, error)
	Unsubscribe(ctx context.Context, url string) (model.SimpleQueryResult, error)
}

// Acceptor принимает апдейт с ожиданием места в очереди (intake.Intake).
type Acceptor interface {
	AcceptWait(ctx context.Context, upd model.Update) error
}

// Options — зависимости и параметры опроса.
type Options struct {
	Source Source
	Intake Acceptor
	// BaseDelay и MaxDelay — пауза после ошибки опроса, удваивается
	// до MaxDelay (MAX_RETRY_BASE_DELAY, MAX_RETRY_MAX_DELAY).
	BaseDelay time.Duration
	MaxDelay  time.Duration
	// CallTimeout — таймаут вызовов снятия подписки (MAX_CALL_TIMEOUT).
	CallTimeout time.Duration
	// Sleep — инжектируемое ожидание (в тестах мгновенное).
	Sleep func(ctx context.Context, d time.Duration) error
	Log   *slog.Logger
}

// Poller опрашивает MAX и передаёт обновления в intake.
type Poller struct {
	opts Options
	log  *slog.Logger
}

func New(opts Options) *Poller {
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	if opts.BaseDelay <= 0 {
		opts.BaseDelay = time.Second
	}
	if opts.MaxDelay < opts.BaseDelay {
		opts.MaxDelay = opts.BaseDelay
	}
	return &Poller{opts: opts, log: opts.Log}
}

// Run опрашивает MAX до отмены контекста. Ошибки опроса не останавливают
// бота: после паузы опрос продолжается.
func (p *Poller) Run(ctx context.Context) error {
	p.dropWebhook(ctx)
	p.log.Info("long polling запущен")

	var marker int64
	delay := p.opts.BaseDelay
	for ctx.Err() == nil {
		updates, next, err := p.opts.Source.GetUpdates(ctx, marker)
		if err != nil {
			if !p.pause(ctx, err, &delay) {
				break
			}
			continue
		}
		delay = p.opts.BaseDelay
		// При остановке marker не сдвигаем: неподтверждённые обновления
		// MAX отдаст после перезапуска.
		if !p.deliver(ctx, updates) {
			break
		}
		if next != 0 {
			marker = next
		}
	}
	p.log.Info("long polling остановлен")
	return nil
}

// deliver передаёт обновления в intake; false — контекст отменён.
func (p *Poller) deliver(ctx context.Context, updates []model.Update) bool {
	for _, upd := range updates {
		if p.opts.Intake.AcceptWait(ctx, upd) != nil {
			return false
		}
	}
	return true
}

// pause ждёт после ошибки опроса и увеличивает задержку;
// false — бот останавливается, опрос прекращается.
func (p *Poller) pause(ctx context.Context, cause error, delay *time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	p.log.Error("long polling: ошибка получения обновлений", "retry_in", *delay, "error", cause)
	if p.opts.Sleep(ctx, *delay) != nil {
		return false
	}
	*delay = min(*delay*2, p.opts.MaxDelay)
	return true
}

// dropWebhook снимает webhook-подписки: с ними MAX не отдаёт /updates.
// Ошибка не фатальна — опрос всё равно запускается, а в логе видна причина.
func (p *Poller) dropWebhook(ctx context.Context) {
	callCtx, cancel := p.callCtx(ctx)
	defer cancel()
	subs, err := p.opts.Source.GetSubscriptions(callCtx)
	if err != nil {
		p.log.Error("long polling: не удалось проверить webhook-подписки", "error", err)
		return
	}
	for _, s := range subs.Subscriptions {
		callCtx, cancel := p.callCtx(ctx)
		_, err := p.opts.Source.Unsubscribe(callCtx, s.URL)
		cancel()
		if err != nil {
			p.log.Error("long polling: не удалось снять webhook-подписку", "error", err)
			continue
		}
		p.log.Warn("long polling: webhook-подписка снята, чтобы получать обновления опросом")
	}
}

func (p *Poller) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if p.opts.CallTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, p.opts.CallTimeout)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
