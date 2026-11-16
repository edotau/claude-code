package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestRouteLine(t *testing.T) {
	r := agent.ModelRoute{Agent: "copilot", Account: "github·business", Model: "gpt-6-astra", Source: "prefix"}
	if got := routeLine(r); got != "copilot · github·business · gpt-6-astra (prefix)" {
		t.Errorf("routeLine = %q", got)
	}
}

func TestGatewayResolveUsage(t *testing.T) {
	if code := cmdGatewayResolve([]string{"claude", "--live"}); code != 2 {
		t.Errorf("--live without --all-profiles exit = %d; want 2", code)
	}
}

// An untyped resolve reports the hop RunQuery picks (the label's resolver), its note kept; an error is returned.
func TestResolveFirstHop(t *testing.T) {
	base, err := os.ReadFile("../providers/defaults.json")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Parse(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got agent.ModelQuery
	hint := ""
	stub := func(_ context.Context, _ *providers.Registry, q agent.ModelQuery, alias string) (agent.ModelQuery, string, error) {
		got, hint = q, alias
		return agent.ModelQuery{Agent: "claude", Model: "ultra:claude-sonnet-5-5"}, "codex is gated out", nil
	}
	old := resolveRunQuery
	t.Cleanup(func() { resolveRunQuery = old })
	resolveRunQuery = stub
	r, _, err := resolveFirstHop(context.Background(), reg, agent.ModelQuery{Agent: "codex"}, "fix the typo")
	if err != nil || got.Agent != "codex" || hint != "haiku" || r.Agent != "claude" || r.Model != "claude-sonnet-5-5" || r.Account != "anthropic·ultra" || !strings.Contains(r.Note, "gated out") {
		t.Errorf("resolveFirstHop = %+v, %v (query %+v); want the stubbed claude sonnet hop on anthropic·ultra with its note", r, err, got)
	}
	resolveRunQuery = func(context.Context, *providers.Registry, agent.ModelQuery, string) (agent.ModelQuery, string, error) {
		return agent.ModelQuery{}, "", errors.New("no route runs codex")
	}
	if _, _, err := resolveFirstHop(context.Background(), reg, agent.ModelQuery{Agent: "codex"}, ""); err == nil {
		t.Error("resolveFirstHop swallowed the ladder error")
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

// --model auto prints the concrete pick and one reason line; json carries at most five ranked candidates.
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
	args := []string{"claude", "--profile", "dev", "--model", "auto", "--task", f}
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

// A model dev's fresh listing lacks prints "served: no"; under --fallback none it exits 2 naming the served ones.
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
		{"", 0, "served: no", ""},
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
