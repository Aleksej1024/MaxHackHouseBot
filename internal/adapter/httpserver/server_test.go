package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePinger struct{ err error }

func (p fakePinger) Ping(context.Context) error { return p.err }

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func healthMux(pg, rd Pinger) *http.ServeMux {
	mux := http.NewServeMux()
	RegisterHealth(mux, pg, rd, discard)
	return mux
}

func get(t *testing.T, h http.Handler, path string) int {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return rr.Code
}

func TestHealthz(t *testing.T) {
	assert.Equal(t, http.StatusOK, get(t, healthMux(fakePinger{}, fakePinger{}), "/healthz"))
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name string
		pg   Pinger
		rd   Pinger
		want int
	}{
		{name: "всё доступно", pg: fakePinger{}, rd: fakePinger{}, want: http.StatusOK},
		{name: "postgres недоступен", pg: fakePinger{err: errors.New("pg down")}, rd: fakePinger{}, want: http.StatusServiceUnavailable},
		{name: "redis недоступен", pg: fakePinger{}, rd: fakePinger{err: errors.New("redis down")}, want: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, get(t, healthMux(tt.pg, tt.rd), "/readyz"))
		})
	}
}

func TestRun_StopsOnCancel(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	srv := New(Options{Addr: addr, Handler: healthMux(fakePinger{}, fakePinger{}), Log: discard})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("сервер не остановился после отмены контекста")
	}
}

func TestRun_ListenError(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	srv := New(Options{Addr: ln.Addr().String(), Handler: http.NewServeMux(), Log: discard})
	require.Error(t, srv.Run(context.Background()), "занятый порт — ошибка, а не тихий выход")
}
