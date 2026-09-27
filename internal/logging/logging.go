// Package logging — единственное место, где выбирается приёмник логов
package logging

import (
	"io"
	"log/slog"
	"os"

	"gopkg.in/natefinch/lumberjack.v2"

	"maxhouse/internal/config"
)

// New создаёт логгер по режиму работы:
// dev — человекочитаемый текст в stdout; prod — JSON в файл с ротацией.
func New(env string, cfg config.Log) *slog.Logger {
	var w io.Writer = os.Stdout
	json := false
	if env == "prod" {
		json = true
		w = &lumberjack.Logger{
			Filename:   cfg.FilePath,
			MaxSize:    cfg.MaxSizeMB,
			MaxBackups: cfg.MaxBackups,
			MaxAge:     cfg.MaxAgeDays,
		}
	}
	opts := &slog.HandlerOptions{Level: cfg.Level, ReplaceAttr: durationAsString}
	if json {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// durationAsString пишет длительности как "20s" или "1.5s", а не числом
// наносекунд (так JSON-обработчик slog выводит time.Duration по умолчанию).
func durationAsString(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindDuration {
		return slog.String(a.Key, a.Value.Duration().String())
	}
	return a
}
