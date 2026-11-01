package opencode

import (
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
)

// stallProvider is an opencode hub provider whose credential resolves through OPENCODE_API_KEY (no stored
// login needed), so the stall tests never depend on a real `opencode auth login`.
func stallProvider() *providers.Provider {
	return &providers.Provider{Name: "opencode", OpencodeID: "opencode",
		Models: map[string]string{models.Opus: "test-model", models.Sonnet: "test-model"}}
}

// opencodeNeverWrites answers --version instantly (so the credential/login probe never hangs) and
// otherwise sleeps without writing a single byte to stdout — the free-dispatch stall fixture.
const opencodeNeverWrites = `
if [ "$1" = "--version" ]; then echo "0.0.0"; exit 0; fi
sleep 5
`

// setupStallFixture isolates opencode's config/data dirs, installs the never-writes fake binary, and
// supplies a credential via the vault-key env var so Available()/Run() never touch a real login.
func setupStallFixture(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("OPENCODE_DB", "")
	pathstest.FakeVendor(t, "opencode", opencodeNeverWrites)
	t.Setenv(OpencodeAPIKeyEnv, "test-key")
}



// opencodeStepStartThenStalls prints only a bookkeeping step_start event (no text/reasoning/tool, no
// output tokens) and then sleeps without further output — proving step_start alone must never disarm
// the stall watchdog (isMeaningfulOpenCodeEvent must reject it).
const opencodeStepStartThenStalls = `
if [ "$1" = "--version" ]; then echo "0.0.0"; exit 0; fi
echo '{"type":"step_start"}'
sleep 5
`

// opencodeTextFast prints a real text event right away, then stays alive past the firstOutput deadline
// before exiting 0 — proving the watchdog actually disarmed (timer.Stop fired) rather than the test
// passing only because the child happened to exit before the deadline could ever fire.
const opencodeTextFast = `
if [ "$1" = "--version" ]; then echo "0.0.0"; exit 0; fi
echo '{"type":"text","part":{"text":"hi"}}'
sleep 0.6
`

// opencodeEmptyTextThenStalls prints a text event with an empty Part.Text (bookkeeping, not real output)
// and then sleeps without further output — proving an empty text event must never disarm the watchdog
// (isMeaningfulOpenCodeEvent must reject it on the TrimSpace(Part.Text) == "" gate).
const opencodeEmptyTextThenStalls = `
if [ "$1" = "--version" ]; then echo "0.0.0"; exit 0; fi
echo '{"type":"text","part":{"text":""}}'
sleep 5
`




