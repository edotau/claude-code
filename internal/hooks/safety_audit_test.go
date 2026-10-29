package hooks

import (
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
)

// A safety ask is audited by its reason and pattern only; the command, which may hold a secret, never is.
func TestSafetyBlockIsAuditedWithoutCommand(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	sql := "DR" + "OP TABLE users_secretmarker" // assembled so the literal never sits in a shell command line
	payload := `{"tool_name":"Bash","tool_input":{"command":"` + sql + `"}}`
	if got, _ := safetyScanDecision(t, strings.NewReader(payload)); got != Ask {
		t.Fatalf("Safety = %d, want ask", got)
	}
	ev, _ := audit.Read(time.Time{})
	if len(ev) != 1 || ev[0].Action != audit.HookBlocked || !strings.HasPrefix(ev[0].Detail, "ask: ") || strings.Contains(ev[0].Detail, "secretmarker") {
		t.Fatalf("audit = %+v", ev)
	}
}
