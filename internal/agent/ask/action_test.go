package ask

import (
	"context"
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
	sctx, rctx, cancel := wholeDeadline(context.Background(), agent.RunOptions{Timeout: time.Minute})
	defer cancel()
	s, _ := sctx.Deadline()
	r, _ := rctx.Deadline()
	if left := time.Until(s); left > time.Minute || left < 59*time.Second {
		t.Errorf("whole deadline in %v, want the 1m timeout", left)
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
	if got := strings.Join(actionArgv("gateway", nil, "", ""), " "); !strings.Contains(got, "--fallback none") {
		t.Errorf("gateway argv %q lacks --fallback none", got)
	}
}
