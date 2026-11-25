package tree

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxyteam"
)

func TestTeamOnlyTreeRenders(t *testing.T) {
	cfg := proxyteam.Config{Teams: []proxyteam.Team{{Name: "phase", Host: proxyteam.HostPhase}}}
	tree := &Tree{Teams: buildTeams(cfg, nil, nil, nil, nil), TeamWarnings: []string{"team config: bad overlay"}}
	var buf bytes.Buffer
	renderTree(&buf, tree, time.Now(), 0, "")
	if out := buf.String(); !strings.Contains(out, "proxy teams") || !strings.Contains(out, "! team config: bad overlay") {
		t.Fatalf("team-only tree hidden:\n%s", out)
	}
}

func TestUnverifiedProxyShown(t *testing.T) {
	cfg := proxyteam.Config{Teams: []proxyteam.Team{{Name: "shared", Host: proxyteam.HostDaemon}}}
	probe := func(int, string, int) proxyState { return proxyUnverified }
	tree := &Tree{Teams: buildTeams(cfg, nil, &proxy.State{PID: 7, Port: 9}, nil, probe)}
	var buf bytes.Buffer
	renderTree(&buf, tree, time.Now(), 0, "")
	if !strings.Contains(buf.String(), "· unverified") || strings.Contains(buf.String(), "stale") {
		t.Fatalf("want unverified only:\n%s", buf.String())
	}
}

func TestProbeProxyClosedPort(t *testing.T) {
	if got := probeProxy(os.Getpid(), "", 1); got != proxyUnverified {
		t.Fatalf("live pid, closed port = %v, want unverified", got)
	}
}

func TestTeamRowsFitWidth(t *testing.T) {
	long := strings.Repeat("x", 120)
	cfg := proxyteam.Config{Teams: []proxyteam.Team{{Name: "phase", Host: proxyteam.HostPhase, Fallback: []string{long}}}}
	scopes := []proxy.Scope{{Team: "phase", ID: "w1", PID: 7, Port: 9, Build: "abc"}}
	ds := []Dispatch{{ID: "d1", Proxy: "phase:w1", Task: long}}
	tree := &Tree{Dispatches: ds, Teams: buildTeams(cfg, scopes, nil, ds, func(int, string, int) proxyState { return proxyLive })}
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
