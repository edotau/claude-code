package ask

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

func TestActionCheckRefusals(t *testing.T) {
	for name, f := range map[string]*askFlags{
		"gateway own": {action: "gateway", actionOwn: []string{"a.go"}, format: "json"},
		"waves":       {action: "gate", waves: "plan.md", format: "json"},
	} {
		if actionCheck(f) == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
	if err := actionCheck(&askFlags{action: "gateway", format: "json"}); err != nil {
		t.Errorf("plain gateway: %v", err)
	}
}

func TestWholeDeadlineCapsAttemptsAndSettle(t *testing.T) {
	sctx, rctx, cancel := wholeDeadline(context.Background(), agent.RunOptions{Timeout: 10 * time.Minute})
	defer cancel()
	s, _ := sctx.Deadline()
	r, _ := rctx.Deadline()
	if left := time.Until(s); left > 10*time.Minute || left < 10*time.Minute-time.Second {
		t.Errorf("whole deadline in %v, want the 10m timeout", left)
	}
	if gap := s.Sub(r); gap != settleGateTimeout {
		t.Errorf("attempts end %v before the settle ends, want %v", gap, settleGateTimeout)
	}
}

func TestActionChildKeepsStderrOutOfAnswer(t *testing.T) {
	out, errb, err := runActionChild(context.Background(), &askFlags{action: "diff"}, "sh", []string{"-c", "echo '{}'; echo noise >&2"})
	if err != nil || out.String() != "{}\n" || errb.String() != "noise\n" {
		t.Errorf("stdout %q stderr %q err %v, want the streams apart", out, errb, err)
	}
}

func TestActionArgvGatewayNeverFallsBack(t *testing.T) {
	if got := strings.Join(actionArgv("gateway", nil, "", "", ""), " "); !strings.Contains(got, "--fallback none") {
		t.Errorf("gateway argv %q lacks --fallback none", got)
	}
}

// A brief's absolute own path becomes relative to the child's workdir, not the dispatcher's cwd.
func TestActionArgvWorkdir(t *testing.T) {
	work := t.TempDir()
	t.Chdir(t.TempDir())
	own := []string{filepath.Join(work, "pkg")}
	for _, action := range []string{"gate", "scope"} {
		if got := actionArgv(action, own, "", "", work); !strings.HasSuffix(strings.Join(got, " "), "--scope pkg") {
			t.Errorf("%s argv = %q", action, got)
		}
		if err := actionCheck(&askFlags{action: action, actionOwn: own, workdir: work, format: "json"}); err != nil {
			t.Errorf("%s check: %v", action, err)
		}
	}
}

// worktree gate exits 0 even when red; a missing or nonzero exit field must fail the action.
func TestRedGate(t *testing.T) {
	for out, fail := range map[string]bool{`{"exit":0}`: false, `{"exit":7}`: true, `{"exit":null}`: true, `{}`: true} {
		if err := redGate([]byte(out)); (err != nil) != fail {
			t.Errorf("redGate(%s) = %v", out, err)
		}
	}
}
