package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/gateway/resolve"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestRouteLine(t *testing.T) {
	r := agent.ModelRoute{Agent: "copilot", Account: "github·business", Model: "gpt-6-astra", Source: "prefix"}
	if got := r.Line(); got != "copilot · github·business · gpt-6-astra (prefix)" {
		t.Errorf("routeLine = %q", got)
	}
}

func TestGatewayResolveUsage(t *testing.T) {
	if code := cmdGatewayResolve([]string{"claude", "--live"}); code != 2 {
		t.Errorf("--live without --all-profiles exit = %d; want 2", code)
	}
	vaulttest.Use(t)
	if _, err := resolve.Query(context.Background(), "claude", "sonnet", "ultra",
		`{"provider":"databricks","profile":"dev","identity":"sp"}`); err == nil {
		t.Error("typed profile accepted another route's identity")
	}
}

// --fallback outside router.FallbackModes is a usage error like the sibling verbs' check, never treated as "not none".
func TestGatewayResolveRejectsUnknownFallback(t *testing.T) {
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdGatewayResolve([]string{"opencode", "--model", "dev:x", "--fallback", "bogus"})
	})
	if code != 2 || !strings.Contains(stderr, `--fallback "bogus" unknown (none|pin|agents|all)`) {
		t.Errorf("exit %d, stderr %q; want exit 2 naming the closed set", code, stderr)
	}
}

// An auto reason in the middle of the note leaves the notes around it joined by one "; ", never an empty part.
func TestPrintRouteAutoReasonMidNote(t *testing.T) {
	choice := &agent.AutoChoice{Tier: "standard", Criterion: "value", Signal: "standard", Model: "m", Fallback: "no ranking"}
	r := agent.ModelRoute{Agent: "opencode", Account: "a", Model: "m", Source: "flag", Note: "first; " + choice.Reason() + "; detail"}
	_, out, _ := hosttest.CaptureOutput(t, func() int { return printRoute(r, choice, "text") })
	if !strings.Contains(out, "note: first; detail\n") {
		t.Errorf("printRoute = %q; want note \"first; detail\"", out)
	}
}

// --task resolves an untyped model to the brief's tier, the same pick the run and the label make.
func TestGatewayResolveTaskTier(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	dir := t.TempDir()
	out := map[string]string{}
	for name, brief := range map[string]string{"fast": "Fix the typo in a.go.", "deep": "Audit the safety-scan rules."} {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, []byte(brief), 0o600); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
			return cmdGatewayResolve([]string{"opencode", "--profile", "dev", "--task", f})
		})
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", name, code, stderr)
		}
		out[name] = stdout
	}
	if out["fast"] == out["deep"] {
		t.Errorf("a typo fix and an audit resolve the same: %q", out["fast"])
	}
}

// An untyped tier pick prints the concrete model and one reason line; json carries at most five ranked candidates.
func TestGatewayResolveAuto(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	s := router.Served{Profile: "dev", FetchedAt: time.Now()}
	for _, id := range []string{"opus-5-5", "sonnet-5-5", "haiku-5-5", "opus-5", "sonnet-5", "sonnet-4-6", "opus-4-8"} {
		s.Models = append(s.Models, router.ServedModel{ID: "databricks-claude-" + id, Task: "llm/v1/chat", Ready: "READY"})
	}
	if err := router.SaveServed(s); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "brief")
	if err := os.WriteFile(f, []byte("Add a --json flag to agents status in a.go and b.go."), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"claude", "--profile", "dev", "--task", f}
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return cmdGatewayResolve(args) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 2 || strings.Contains(lines[0], " auto ") || !strings.HasPrefix(lines[1], "auto: standard (") {
		t.Fatalf("exit %d: %q (%s), want the route line then one auto: reason line", code, out, stderr)
	}
	code, out, _ = hosttest.CaptureOutput(t, func() int { return cmdGatewayResolve(append(args, "--format", "json")) })
	var doc struct {
		Auto struct{ Ranked []json.RawMessage } `json:"auto"`
	}
	if err := json.Unmarshal([]byte(out), &doc); code != 0 || err != nil || len(doc.Auto.Ranked) == 0 || len(doc.Auto.Ranked) > 5 {
		t.Errorf("json exit %d (%v): %d ranked, want 1..5: %s", code, err, len(doc.Auto.Ranked), out)
	}
}

// A typed model dev's fresh listing lacks exits 2 naming the served ones, any --fallback (R8: AcceptModel refuses).
func TestGatewayResolveUnservedModel(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	if err := router.SaveServed(router.Served{Profile: "dev", FetchedAt: time.Now(), Models: []router.ServedModel{
		{ID: "databricks-gemini-3-8-flash", Ready: "READY"}}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		extra    string
		code     int
		out, err string
	}{
		{"", 2, "", "databricks-gemini-3-8-flash"},
		{"none", 2, "", "databricks-gemini-3-8-flash"},
	} {
		args := []string{"gemini", "--profile", "dev", "--model", "gemini-4-argon-high"}
		if tc.extra != "" {
			args = append(args, "--fallback", tc.extra)
		}
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdGatewayResolve(args) })
		if code != tc.code || !strings.Contains(stdout, tc.out) || !strings.Contains(stderr, tc.err) {
			t.Errorf("--fallback %q: exit %d, stdout %q, stderr %q; want exit %d with %q / %q", tc.extra, code, stdout, stderr, tc.code, tc.out, tc.err)
		}
	}
}

// An unverified --all-profiles row prints served: unverified once, on its route line.
func TestResolveAllUnverifiedOnce(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"auth":{"type":"oauth"},
		"host":"https://dev.cloud.databricks.com","preset":"databricks"}}}}`)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	rows := resolve.All(context.Background(), reg, agent.ModelQuery{Agent: "opencode", Model: "opus", Identity: "sp", Account: "parent"}, false)
	for _, row := range rows {
		if row.Route != nil && row.Route.Identity != "" {
			t.Error("all-profiles inherited the input route's identity")
		}
	}
	_, out, _ := hosttest.CaptureOutput(t, func() int { return printRows(rows, "text") })
	if n := strings.Count(out, "unverified"); !strings.Contains(out, "dev") || n != 1 {
		t.Errorf("printRows = %q; want the dev row with served: unverified once (got %d)", out, n)
	}
}
