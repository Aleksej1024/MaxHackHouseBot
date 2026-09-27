package maxbot

import (
	"context"
	"errors"
	"math/rand"
	"time"

	maxbotapi "github.com/max-messenger/max-bot-api-client-go/v2"
)

// RetryOptions — параметры повторов вызовов MAX.
type RetryOptions struct {
	Attempts    int           // максимум попыток (MAX_RETRY_ATTEMPTS)
	BaseDelay   time.Duration // базовая задержка, удваивается (MAX_RETRY_BASE_DELAY)
	MaxDelay    time.Duration // верхняя граница задержки (MAX_RETRY_MAX_DELAY)
	CallTimeout time.Duration // таймаут одной попытки (MAX_CALL_TIMEOUT)
	// Sleep — инжектируемая задержка между попытками (в тестах мгновенная).
	// По умолчанию ожидание с учётом отмены контекста.
	Sleep func(ctx context.Context, d time.Duration) error
}

// retrier выполняет вызовы MAX с экспоненциальной задержкой.
// Повторяются только временные ошибки библиотеки: TimeoutError и NetworkError.
// maxbotapi.Error не несёт HTTP-статус (v2.4.0) и считается постоянной
type retrier struct {
	opts RetryOptions
}

func newRetrier(opts RetryOptions) *retrier {
	if opts.Attempts <= 0 {
		opts.Attempts = 1
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	return &retrier{opts: opts}
}

// do вызывает fn с повторами. Каждая попытка получает контекст
// с таймаутом CallTimeout от родительского ctx.
func (r *retrier) do(ctx context.Context, fn func(ctx context.Context) error) error {
	var err error
	for attempt := 1; attempt <= r.opts.Attempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, r.opts.CallTimeout)
		err = fn(attemptCtx)
		cancel()

		if err == nil || !isTemporary(err) || attempt == r.opts.Attempts {
			return err
		}
		if sleepErr := r.opts.Sleep(ctx, r.delay(attempt)); sleepErr != nil {
			return sleepErr
		}
	}
	return err
}

// delay — задержка перед попыткой attempt+1: BaseDelay * 2^(attempt-1),
// ограниченная MaxDelay, с разбросом до +10%.
func (r *retrier) delay(attempt int) time.Duration {
	d := r.opts.BaseDelay
	for i := 1; i < attempt; i++ {
		d *= 2
		if r.opts.MaxDelay > 0 && d >= r.opts.MaxDelay {
			d = r.opts.MaxDelay
			break
		}
	}
	if r.opts.MaxDelay > 0 && d > r.opts.MaxDelay {
		d = r.opts.MaxDelay
	}
	if d > 0 {
		d += time.Duration(rand.Int63n(int64(d)/10 + 1))
	}
	return d
}

func isTemporary(err error) bool {
	var timeoutErr *maxbotapi.TimeoutError
	if errors.As(err, &timeoutErr) {
		return true
	}
	var networkErr *maxbotapi.NetworkError
	return errors.As(err, &networkErr)
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
