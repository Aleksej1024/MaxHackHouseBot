// Package httpserver — HTTP-сервер бот. 
// Работает в обоих режимах приёма обновлений: /healthz и /readyz нужны всегда
// (health-check контейнера), обработчик /webhook подключается снаружи
// только в режиме webhook.
package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	defaultShutdownTimeout = 10 * time.Second
	readyzTimeout          = 2 * time.Second
)

// Pinger проверяет доступность зависимости для /readyz.
type Pinger interface {
	Ping(ctx context.Context) error
}

// RegisterHealth добавляет /healthz (процесс жив) и /readyz (доступны
// Postgres и Redis).
func RegisterHealth(mux *http.ServeMux, postgres, redis Pinger, log *slog.Logger) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
		defer cancel()
		if err := postgres.Ping(ctx); err != nil {
			log.Error("readyz: postgres недоступен", "error", err)
			http.Error(w, "postgres unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := redis.Ping(ctx); err != nil {
			log.Error("readyz: redis недоступен", "error", err)
			http.Error(w, "redis unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// Options — параметры сервера.
type Options struct {
	Addr    string
	Handler http.Handler
	// ShutdownTimeout — сколько ждать завершения текущих запросов при
	// остановке (SHUTDOWN_TIMEOUT).
	ShutdownTimeout time.Duration
	Log             *slog.Logger
}

// Server — HTTP-сервер с корректной остановкой.
type Server struct {
	srv             *http.Server
	shutdownTimeout time.Duration
	log             *slog.Logger
}

func New(opts Options) *Server {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	timeout := opts.ShutdownTimeout
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}
	return &Server{
		srv: &http.Server{
			Addr:              opts.Addr,
			Handler:           opts.Handler,
			ReadHeaderTimeout: 5 * time.Second,
		},
		shutdownTimeout: timeout,
		log:             log,
	}
}

// Run слушает адрес до отмены контекста, затем дожидается текущих запросов.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		s.log.Info("останавливаем HTTP-сервер")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			s.log.Error("ошибка остановки HTTP-сервера", "error", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
