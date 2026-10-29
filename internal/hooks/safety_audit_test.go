package hooks

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/audit"
)

// A safety block is audited by its reason and pattern only; the command, which may hold a secret, never is.
func TestSafetyBlockIsAuditedWithoutCommand(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	sql := "DR" + "OP TABLE users_secretmarker" // assembled so the literal never sits in a shell command line
	payload := `{"tool_name":"Bash","tool_input":{"command":"` + sql + `"}}`
	if code := SafetyScan(strings.NewReader(payload), io.Discard, io.Discard); code != ExitBlock {
		t.Fatalf("Safety = %d, want block", code)
	}
	ev, _ := audit.Read(time.Time{})
	if len(ev) != 1 || ev[0].Action != audit.HookBlocked || strings.Contains(ev[0].Detail, "secretmarker") {
		t.Fatalf("audit = %+v", ev)
	}
}
