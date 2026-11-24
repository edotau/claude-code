package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxyteam"
)

func TestChosenProxyIsTakenOnceAndForgotten(t *testing.T) {
	ctx := context.WithValue(context.Background(), dispatchIDKey{}, "d-take")
	routeMu.Lock()
	chosenByID["d-take"] = "phase:x-w1"
	routeMu.Unlock()
	forgetChosenProxy(ctx)
	if got := TakeChosenProxy("d-take"); got != "" {
		t.Fatalf("forgotten label = %q, want empty", got)
	}
	routeMu.Lock()
	chosenByID["d-take"] = "phase:x-w1"
	routeMu.Unlock()
	if got, again := TakeChosenProxy("d-take"), TakeChosenProxy("d-take"); got != "phase:x-w1" || again != "" {
		t.Fatalf("take = %q then %q", got, again)
	}
}

func TestRouteWalkSkipsDeadFirstRoute(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	dead := cmd.ProcessState.Pid()
	cands := []routeCand{
		{team: proxyteam.Team{Name: "grp", Host: proxyteam.HostPhase}, scope: proxy.Scope{Team: "grp", PID: dead, Start: "x", Port: 1}},
		{team: proxyteam.Team{Name: "shared", Host: proxyteam.HostDaemon}, scope: proxy.Scope{Team: "shared"}},
	}
	ensure := func(context.Context) (string, error) { return "http://127.0.0.1:9", nil }
	got, base, note, err := chooseRoute(context.Background(), "pi", cands, nil, ensure)
	if err != nil {
		t.Fatal(err)
	}
	if got.team.Name != "shared" || base != "http://127.0.0.1:9" {
		t.Fatalf("chose %s at %s", got.team.Name, base)
	}
	if !strings.HasPrefix(note, "pi: proxy: grp") || !strings.HasSuffix(note, "using shared") {
		t.Fatalf("note %q", note)
	}
	cands[0].scope = proxy.Scope{Team: "grp"}
	_, _, note, err = chooseRoute(context.Background(), "pi", cands, map[string]bool{"grp": true}, ensure)
	if err != nil || !strings.Contains(note, "failed mid-run") {
		t.Fatalf("skipped route: note %q err %v", note, err)
	}
}
