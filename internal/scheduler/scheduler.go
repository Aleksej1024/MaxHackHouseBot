// Package scheduler — внутренний планировщик (тикер), запускающий
// периодические задачи usecase/jobs в том же процессе.
package scheduler

import (
	"context"
	"log/slog"
	"time"
)

// Job — периодическая задача. Ошибка задачи логируется и не останавливает
// планировщик: следующий тик попробует снова.
type Job struct {
	Name string
	Run  func(ctx context.Context) error
}

type Runner struct {
	interval time.Duration
	jobs     []Job
	log      *slog.Logger
}

func New(interval time.Duration, log *slog.Logger, jobs ...Job) *Runner {
	return &Runner{interval: interval, jobs: jobs, log: log}
}

// Run тикает с заданным интервалом до отмены контекста.
func (r *Runner) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// tick выполняет задачи по очереди.
func (r *Runner) tick(ctx context.Context) {
	for _, job := range r.jobs {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		if err := job.Run(ctx); err != nil {
			r.log.Error("задача планировщика завершилась с ошибкой",
				"job", job.Name, "duration", time.Since(start), "error", err)
		}
	}
}
