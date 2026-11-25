package tree

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxyteam"
)

func TestTeamRowsFitWidth(t *testing.T) {
	long := strings.Repeat("x", 120)
	cfg := proxyteam.Config{Teams: []proxyteam.Team{{Name: "phase", Host: proxyteam.HostPhase, Fallback: []string{long}}}}
	scopes := []proxy.Scope{{Team: "phase", ID: "w1", PID: 7, Port: 9, Build: "abc"}}
	ds := []Dispatch{{ID: "d1", Proxy: "phase:w1", Task: long}}
	tree := &Tree{Dispatches: ds, Teams: buildTeams(cfg, scopes, nil, ds, func(int, string) bool { return true })}
	var buf bytes.Buffer
	renderTree(&buf, tree, time.Now(), 0, "")
	out := buf.String()
	if !strings.Contains(out, "proxy teams") || !strings.Contains(out, "· d1 ") || !strings.Contains(out, "1 in flight") {
		t.Fatalf("team view missing:\n%s", out)
	}
	after := out[strings.Index(out, "proxy teams"):]
	for line := range strings.SplitSeq(strings.TrimSpace(after), "\n") {
		if n := len([]rune(line)); n > 80 {
			t.Errorf("row is %d columns: %q", n, line)
		}
	}
}
