package config

import (
	"fmt"
	"os"
	"regexp"
	"testing"
)

// TestMain изолирует тесты от окружения разработчика: переменные бота,
// экспортированные в shell (например, из .env), иначе подменяли бы
// значения по умолчанию. Имена берутся из исходника config.go, поэтому
// список не устаревает при добавлении новых переменных.
func TestMain(m *testing.M) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		fmt.Fprintln(os.Stderr, "чтение config.go:", err)
		os.Exit(1)
	}
	for _, match := range regexp.MustCompile(`(?:env\w*|required)\("([A-Z][A-Z0-9_]+)"`).FindAllSubmatch(src, -1) {
		_ = os.Unsetenv(string(match[1]))
	}
	os.Exit(m.Run())
}
