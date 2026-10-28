package agent

import (
	"encoding/base64"
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// TestMain drops the session's provider/model pins and points the config dir and vault key at a throwaway dir:
// a test must never resolve the developer's live provider or reach the live router secret.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agent-test-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("CLAUDE_CONFIG_DIR", dir)
	_ = os.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	for _, k := range []string{providers.PinProfile, providers.PinModel} {
		_ = os.Unsetenv(k)
	}
	for _, slot := range models.Slots {
		_ = os.Unsetenv(providers.SlotPin(slot))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
