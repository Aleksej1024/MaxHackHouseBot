package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTick_RunsAllJobsDespiteErrors(t *testing.T) {
	var order []string
	r := New(time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)),
		Job{Name: "a", Run: func(context.Context) error { order = append(order, "a"); return errors.New("boom") }},
		Job{Name: "b", Run: func(context.Context) error { order = append(order, "b"); return nil }},
	)
	r.tick(context.Background())
	assert.Equal(t, []string{"a", "b"}, order, "ошибка задачи не мешает следующим")
}

func TestTick_StopsOnCancelledContext(t *testing.T) {
	called := false
	r := New(time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)),
		Job{Name: "a", Run: func(context.Context) error { called = true; return nil }})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.tick(ctx)
	assert.False(t, called)
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	r := New(time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run вернул ошибку: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run не завершился после отмены контекста")
	}
}
