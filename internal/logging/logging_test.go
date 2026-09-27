package logging

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maxhouse/internal/config"
)

func TestNew_DevTextToStdout(t *testing.T) {
	log := New("dev", config.Log{Level: slog.LevelDebug})
	require.NotNil(t, log)
	log.Debug("smoke")
}

func TestNew_ProdJSONToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.log")
	log := New("prod", config.Log{
		Level:      slog.LevelDebug,
		FilePath:   path,
		MaxSizeMB:  1,
		MaxBackups: 1,
		MaxAgeDays: 1,
	})
	log.Info("smoke", "chat_id", int64(1), "timeout", 20*time.Second)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"msg":"smoke"`)
	assert.Contains(t, string(data), `"timeout":"20s"`, "длительность строкой, а не наносекундами")
	assert.True(t, strings.HasPrefix(strings.TrimSpace(string(data)), "{"))
}
