package opencode

import (
	"testing"

	"github.com/edotau/claude-code/internal/agent/retry"
)

// Only a quota on the root run's primary stream is a cause; opencode's own title or small-model call is not.
func TestLimitCauseRootStreamOnly(t *testing.T) {
	const head = `timestamp=2026-11-19T19:34:58.878Z level=ERROR run=r1 message="stream error" providerID=opencode-go modelID=kimi-k3 session.id=ses_x `
	const quota = `error.error="AI_APICallError: OpenCode Go usage limit reached (monthly, resets in 5d 7h). To keep going now, see https://example.invalid/go."`
	for _, c := range []struct {
		fields string
		want   bool
	}{
		{"small=false agent=build mode=primary ", true},
		{"small=true agent=title mode=primary ", false},
	} {
		got := limitCause(head + c.fields + quota)
		if (got != nil) != c.want || got != nil && got.Kind != retry.KindQuota {
			t.Errorf("%s: cause = %v, want quota %v", c.fields, got, c.want)
		}
	}
}

// A quota line ends the child before any output and is ignored once the root run has produced output.
func TestLimitWatchEndsOnlyBeforeOutput(t *testing.T) {
	const line = `level=ERROR message="stream error" small=false mode=primary error.error="usage limit reached (monthly, resets in 5d)"` + "\n"
	for _, spoke := range []bool{false, true} {
		killed := 0
		w := &limitWatch{kill: func() error { killed++; return nil }}
		if spoke {
			w.markSpoke()
		}
		_, _ = w.Write([]byte(line))
		if got := killed == 1 && w.Err() != nil; got == spoke {
			t.Errorf("spoke=%v: killed=%d err=%v", spoke, killed, w.Err())
		}
	}
}
