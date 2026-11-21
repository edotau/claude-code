package cli

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// configure agents refuses a bad key, agent or profile, writes nothing on --dry-run, and writes the record otherwise.
func TestConfigureRecord(t *testing.T) {
	hosttest.PinScratch(t)
	for _, args := range [][]string{
		{"codex", "bogus=1"}, {"nope", "model=x"}, {"codex", "fallback=nosuch"},
		{"codex", "config_route=nosuch"}, {"gemini", "config_route=session"},
	} {
		if code := cmdConfigure(append([]string{"agents"}, args...)); code == 0 {
			t.Errorf("record %v exited 0, want a refusal", args)
		}
	}
	if code := cmdConfigure([]string{"agents", "codex", "model=auto", "--dry-run"}); code != 0 || vault.GetAgentRecord("codex").Model != "" {
		t.Fatalf("dry-run = exit %d, model %q, want 0 and nothing written", code, vault.GetAgentRecord("codex").Model)
	}
	if code := cmdConfigure([]string{"agents", "codex", "model=auto", "--check"}); code != 0 || vault.GetAgentRecord("codex").Model != "" {
		t.Fatalf("--check = exit %d, model %q, want 0 and nothing written", code, vault.GetAgentRecord("codex").Model)
	}
	if code := cmdConfigure([]string{"agents", "codex", "model=auto"}); code != 0 || vault.GetAgentRecord("codex").Model != "auto" {
		t.Errorf("record = exit %d, model %q, want 0 and auto", code, vault.GetAgentRecord("codex").Model)
	}
}

// profile=qa stores the route object with the provider from qa's row and no legacy profile key.
func TestConfigureRecordProfileWritesRoute(t *testing.T) {
	hosttest.PinScratch(t)
	vaulttest.UseFreshKey(t)
	row := map[string]any{"provider": "databricks", "preset": "databricks", "host": "https://example.cloud.databricks.com", "auth": map[string]any{"type": "pat"}}
	if err := providers.SetProviderFields("qa", row); err != nil {
		t.Fatal(err)
	}
	if code := cmdConfigure([]string{"agents", "codex", "profile=qa"}); code != 0 {
		t.Fatalf("profile=qa exit %d, want 0", code)
	}
	if want := (route.Route{Provider: "databricks", Profile: "qa"}); vault.GetAgentRecord("codex").Route != want {
		t.Errorf("record route %+v, want %+v", vault.GetAgentRecord("codex").Route, want)
	}
	var raw string
	_ = vault.View(func(tx *vault.Tx) error { raw, _ = tx.Get(vault.Overlay, vault.AgentRecordsKey); return nil })
	if !strings.Contains(raw, `"route":{"provider":"databricks","profile":"qa"}`) || strings.Contains(raw, `"profile":"qa",`) {
		t.Errorf("stored %s, want the route object only", raw)
	}
}

// config_route is record-only: a dry run writes nothing, a real run stores it in the codex record's extras.
func TestConfigureRecordRoute(t *testing.T) {
	hosttest.PinScratch(t)
	if code := cmdConfigure([]string{"agents", "codex", "config_route=session", "--dry-run"}); code != 0 ||
		vault.GetAgentRecord("codex").Extras["config_route"] != "" {
		t.Fatalf("config_route dry-run = exit %d, want 0 and nothing written", code)
	}
	if code := cmdConfigure([]string{"agents", "codex", "config_route=session"}); code != 0 ||
		vault.GetAgentRecord("codex").Extras["config_route"] != "session" {
		t.Errorf("config_route = exit %d, extras %v, want 0 and session", code, vault.GetAgentRecord("codex").Extras)
	}
}

// A declared extras key goes through the same verb: set, validator refusal, dry-run, clear, unknown key.
func TestConfigureRecordExtras(t *testing.T) {
	hosttest.PinScratch(t)
	run := func(args ...string) int { return cmdConfigure(append([]string{"agents", "pi"}, args...)) }
	pkgs := func() string { return vault.GetAgentRecord("pi").Extras["packages"] }
	if run("packages=npm:foo@^1") == 0 || run("bogus=1") == 0 {
		t.Error("a floating version or an undeclared key exited 0")
	}
	if run("packages=npm:foo@1.2.3", "--dry-run") != 0 || pkgs() != "" {
		t.Fatalf("dry-run wrote %q", pkgs())
	}
	if run("packages=npm:foo@1.2.3") != 0 || pkgs() != "npm:foo@1.2.3" {
		t.Fatalf("set stored %q", pkgs())
	}
	if run("packages=") != 0 || pkgs() != "" {
		t.Errorf("clear left %v", vault.GetAgentRecord("pi").Extras)
	}
}

// A control character never reaches a record, and a dispatched leaf cannot pin or approve a pi package.
func TestConfigureRecordExtrasRefusals(t *testing.T) {
	hosttest.PinScratch(t)
	if cmdConfigure([]string{"agents", "codex", "sandboxMode=workspace-write\nx"}) == 0 {
		t.Error("a control character in an extras value was stored")
	}
	t.Setenv(proc.DispatchIDEnv, "d-1")
	if cmdConfigure([]string{"agents", "pi", "packages_approved=npm:foo@1.2.3"}) == 0 || vault.GetAgentRecord("pi").Extras["packages_approved"] != "" {
		t.Error("a dispatched leaf approved a pi package")
	}
}

// An empty value is stored, so it keeps unsetting a built-in: clearing sandboxMode must not restore danger-full-access.
func TestConfigureRecordEmptyExtraUnsetsBuiltin(t *testing.T) {
	hosttest.PinScratch(t)
	if code := cmdConfigure([]string{"agents", "codex", "sandboxMode="}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	ex, ok := vault.GetAgentRecord("codex").Extras["sandboxMode"]
	rec, err := agentconfig.Settings("codex")
	if !ok || ex != "" || err != nil || rec.Extras["sandboxMode"] != "" {
		t.Errorf("stored %q (present %v), merged %q (%v); want the built-in unset", ex, ok, rec.Extras["sandboxMode"], err)
	}
}

// A model is judged on the route set beside it, not the stored one.
func TestRecordProfileJudgesOnNewRoute(t *testing.T) {
	hosttest.PinScratch(t)
	vaulttest.SetRecord(t, "codex", "profile", "old")
	set := map[string]string{"MODEL": "x", "ROUTE": `{"provider":"databricks","profile":"qa"}`}
	if got := recordProfile("codex", set); got != "qa" {
		t.Errorf("recordProfile = %q, want qa (the route being written)", got)
	}
}

// A refused model is a usage error, exit 2, and writes nothing.
func TestConfigureRecordModelRefusalExitsUsage(t *testing.T) {
	hosttest.PinScratch(t)
	if code := cmdConfigure([]string{"agents", "codex", "model=bogus:opus"}); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
}

// A profile prefix on a model is refused (exit 2) naming the row's overlay; a local tag such as qwen3:30b passes.
func TestModelPrefixRefused(t *testing.T) {
	seedAltRows(t)
	want := `use --overlay '{"provider":"anthropic","profile":"alt2"}' --model`
	cases := []struct {
		name string
		run  func() int
	}{
		{"configure", func() int { return cmdConfigure([]string{"agents", "codex", "model=alt2:opus", "--dry-run"}) }},
		{"resolve", func() int { return cmdGatewayResolve([]string{"codex", "--model", "alt2:auto"}) }},
		{"label", func() int { return cmdAgentsLabel([]string{"codex", "--model", "alt2:opus"}) }},
	}
	for _, tc := range cases {
		code, _, stderr := hosttest.CaptureOutput(t, tc.run)
		if code != 2 || !strings.Contains(stderr, want) {
			t.Errorf("%s: exit %d, stderr %q; want 2 naming the overlay form", tc.name, code, stderr)
		}
	}
	if err := refuseModelPrefix("qwen3:30b"); err != nil {
		t.Errorf("qwen3:30b refused: %v", err)
	}
}
