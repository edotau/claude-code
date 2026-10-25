package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/route"
)

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureRouteStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := fn()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), code
}

// TestRouteVerbJSON: the pick's JSON carries weights, per-dimension scores, totals and each skip's gate.
func TestRouteVerbJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("COPILOT_GITHUB_USER", "work-user")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"copilot", "codex"} {
		if err := os.WriteFile(filepath.Join(dir, "bin", b), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	routeRefresh = func(context.Context, string) bool { return false }
	oldGH := route.GHAccounts
	route.GHAccounts = func() []string { return []string{"work-user"} }
	t.Cleanup(func() { route.GHAccounts = oldGH })
	t.Cleanup(func() { routeRefresh = nil })
	if err := route.MarkExhausted("copilot:work-user", time.Now().Add(240*time.Hour), time.Now(), "402"); err != nil {
		t.Fatal(err)
	}
	out, code := captureRouteStdout(t, func() int { return cmdRoute([]string{"--tier", "sonnet", "--agent", "copilot", "--format", "json"}) })
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var res route.Result
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if res.Tier != "sonnet" || res.Weights["price"] == 0 || len(res.Candidates) == 0 {
		t.Fatalf("res = %+v", res)
	}
	var skip *route.Skip
	for i := range res.Skipped {
		if res.Skipped[i].Runner == "copilot" {
			skip = &res.Skipped[i]
		}
	}
	if skip == nil || skip.Gate != route.GateExhausted || !strings.Contains(skip.Reason, "copilot:work-user exhausted until") {
		t.Errorf("copilot skip = %+v in %+v", skip, res.Skipped)
	}
	if c := res.Candidates[len(res.Candidates)-1]; c.Runner != "claude" || c.Model != "sonnet" || c.Scores == nil {
		t.Errorf("last candidate = %+v, want claude at sonnet with scores", c)
	}
	if code := cmdRoute([]string{"--tier", "fable"}); code == 0 {
		t.Error("an unrouted tier must fail")
	}
}
