package agent

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// TestMain drops the session's provider/model pins: a test must never resolve the developer's live provider.
func TestMain(m *testing.M) {
	for _, k := range []string{providers.PinProvider, providers.PinModel} {
		_ = os.Unsetenv(k)
	}
	for _, slot := range models.Slots {
		_ = os.Unsetenv(providers.SlotPin(slot))
	}
	os.Exit(m.Run())
}
