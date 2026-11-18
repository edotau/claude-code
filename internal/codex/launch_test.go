package codex

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCodexStatusScrubsVaultKey(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	t.Setenv("HARNESS_VAULT_KEY", "probe-secret")
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"key=$HARNESS_VAULT_KEY\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := codexStatus(context.Background(), bin)
	if err != nil || strings.Contains(string(out), "probe-secret") {
		t.Fatalf("codex login status saw the vault key (err=%v): %q", err, out)
	}
}

// Tool runs stay sealed by default: network only with overlay codexNetwork, never read-only, never inside a dispatch.
func TestCodexHeadlessNetworkOptIn(t *testing.T) {
	vaulttest.Use(t)
	putCodexRecord(t, vault.AgentRecord{Extras: map[string]string{extraSandboxMode: "workspace-write"}})
	const net = "sandbox_workspace_write.network_access=true"
	open := proc.WithValues(context.Background(), proc.Values{agentconfig.OverlayVar: `{"codexNetwork":true}`})
	if got := codexHeadless(context.Background(), nil, &agent.Headless{Tools: true}); slices.Contains(got, net) {
		t.Fatalf("default tools run must stay sealed: %q", got)
	}
	if got := codexHeadless(open, nil, &agent.Headless{Tools: true}); !slices.Contains(got, net) {
		t.Fatalf("opted-in tools run lacks %s: %q", net, got)
	}
	if got := codexHeadless(open, nil, &agent.Headless{}); slices.Contains(got, net) {
		t.Fatalf("read-only run must stay sealed even when opted in: %q", got)
	}
	_, notes, remove, err := codexToolsEnv(open, nil, true, "workspace-write")
	remove()
	if err != nil || !slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "codexNetwork") }) {
		t.Errorf("launch notes must show the opened network: %v, %v", notes, err)
	}
	nested := proc.WithValues(context.Background(), proc.Values{agentconfig.OverlayVar: `{"codexNetwork":true}`, proc.DispatchIDEnv: "d1"})
	if _, _, remove, err := codexToolsEnv(nested, nil, true, "workspace-write"); err == nil {
		remove()
		t.Error("a nested dispatch must not open network for itself")
	}
	depthOnly := proc.WithValues(context.Background(), proc.Values{agentconfig.OverlayVar: `{"codexNetwork":true}`, proc.DispatchDepthEnv: "1"})
	if _, _, remove, err := codexToolsEnv(depthOnly, nil, true, "workspace-write"); err == nil {
		remove()
		t.Error("a dispatch child with only its depth marker must not open network for itself")
	}
}

// Tools sandbox: record sandboxMode (empty: workspace-write), bad value refused; no tools is read-only.
func TestCodexSandboxOptIn(t *testing.T) {
	vaulttest.Use(t)
	putCodexRecord(t, vault.AgentRecord{Extras: map[string]string{extraSandboxMode: "danger-full-access"}})
	if got := codexHeadless(context.Background(), nil, &agent.Headless{Tools: true}); !slices.Contains(got, "danger-full-access") {
		t.Fatalf("tools run ignored the record's sandboxMode: %q", got)
	}
	if got := codexHeadless(context.Background(), nil, &agent.Headless{}); !slices.Contains(got, "read-only") {
		t.Fatalf("a tools-off run must stay read-only: %q", got)
	}
	putCodexRecord(t, vault.AgentRecord{Extras: map[string]string{extraSandboxMode: ""}})
	if got := codexHeadless(context.Background(), nil, &agent.Headless{Tools: true}); !slices.Contains(got, "workspace-write") {
		t.Fatalf("empty record sandboxMode must give workspace-write: %q", got)
	}
	putCodexRecord(t, vault.AgentRecord{Extras: map[string]string{extraSandboxMode: "read-only"}})
	if _, err := codexSandbox(true); err == nil {
		t.Fatal("an invalid record sandboxMode must be refused")
	}
}

// Go caches land under a temp dir only where the caller set none, and only under workspace-write.
func TestCodexToolsEnvCaches(t *testing.T) {
	for _, k := range []string{"GOCACHE", "GOMODCACHE", "GOLANGCI_LINT_CACHE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	env, notes, remove, err := codexToolsEnv(context.Background(), []string{"GOMODCACHE=/mine"}, true, "workspace-write")
	if err != nil || len(notes) != 0 {
		t.Fatalf("default run: notes %q err %v; want no note", notes, err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GOCACHE=") || !strings.Contains(joined, "GOLANGCI_LINT_CACHE=") || strings.Count(joined, "GOMODCACHE=") != 1 {
		t.Fatalf("caches: want GOCACHE and lint added, caller's GOMODCACHE kept alone:\n%s", joined)
	}
	dir := filepath.Dir(strings.TrimPrefix(env[1], "GOCACHE="))
	remove()
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("cleanup left %s", dir)
	}
}

func TestCodexToolsEnvKeepsCallerCaches(t *testing.T) {
	t.Setenv("GOCACHE", "/process")
	env, _, remove, _ := codexToolsEnv(context.Background(), nil, true, "workspace-write")
	defer remove()
	if strings.Contains(strings.Join(env, "\n"), "GOCACHE=") {
		t.Errorf("a process-set GOCACHE was overridden: %d", len(env))
	}
	if env, _, _, _ := codexToolsEnv(context.Background(), nil, true, "danger-full-access"); len(env) != 0 {
		t.Errorf("full access gets no cache env: %d", len(env))
	}
}

// A run's snapshot decides the network opt-in and caller caches, whatever the process environment says.
func TestCodexHeadlessReadsRunValues(t *testing.T) {
	vaulttest.Use(t)
	putCodexRecord(t, vault.AgentRecord{Extras: map[string]string{extraSandboxMode: "workspace-write"}})
	t.Setenv("GOCACHE", "/ambient")
	ctx := proc.WithValues(context.Background(), proc.Values{agentconfig.OverlayVar: `{"codexNetwork":true}`, "GOCACHE": "/run"})
	got := codexHeadless(ctx, nil, &agent.Headless{Tools: true})
	if !slices.Contains(got, "sandbox_workspace_write.network_access=true") {
		t.Fatalf("run snapshot overlay network opt-in ignored: %q", got)
	}
	env, _, remove, _ := codexToolsEnv(ctx, nil, true, "workspace-write")
	defer remove()
	if strings.Contains(strings.Join(env, "\n"), "GOCACHE=") {
		t.Errorf("a run-snapshot GOCACHE was overridden: %d", len(env))
	}
	ctx = proc.WithValues(context.Background(), proc.Values{})
	if env, _, remove, _ := codexToolsEnv(ctx, nil, true, "workspace-write"); !strings.Contains(strings.Join(env, "\n"), "GOCACHE=") {
		t.Errorf("an ambient GOCACHE leaked into a run whose snapshot has none: %d", len(env))
	} else {
		remove()
	}
}

func TestCodexFileOverridesQuotedClosingBracket(t *testing.T) {
	for _, table := range []string{`model_providers."prod]blue"`, `model_providers.'prod]blue'`} {
		t.Run(table, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			body := "[" + table + "] # provider\nname = \"blue\"\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := codexFileOverrides(path)
			if err != nil {
				t.Fatalf("valid quoted table header rejected: %v", err)
			}
			want := []string{"-c", table + `.name="blue"`}
			if !slices.Equal(got, want) {
				t.Fatalf("quoted table override: got %q, want %q", got, want)
			}
		})
	}
}

func TestCodexFileOverridesLongLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	value := `"` + strings.Repeat("x", 80<<10) + `"`
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[model_provider]\ninstructions = "+value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := codexFileOverrides(path)
	if err != nil {
		t.Fatalf("valid config line over 64 KiB: %v", err)
	}
	want := []string{"-c", "model_provider.instructions=" + value}
	if !slices.Equal(got, want) {
		t.Fatalf("long config value did not round-trip as a table-qualified -c override (got %d args)", len(got))
	}
}

// A tools-off exec drops the skill list, plugins and app connectors; a tools run keeps every one of them.
func TestCodexHeadlessToolsOffTrimsPrompt(t *testing.T) {
	off, on := codexHeadless(context.Background(), nil, &agent.Headless{}), codexHeadless(context.Background(), nil, &agent.Headless{Tools: true})
	for _, c := range []string{"skills.include_instructions=false", "features.plugins=false", "features.apps=false"} {
		if !slices.Contains(off, c) || slices.Contains(on, c) {
			t.Errorf("%s: tools-off has it = %v, tools run has it = %v; want true, false", c, slices.Contains(off, c), slices.Contains(on, c))
		}
	}
	if !slices.Contains(off, "read-only") {
		t.Errorf("tools-off exec lost its read-only sandbox: %q", off)
	}
}

// A headless exec is a leaf: it drops the sub-agent tools and request_user_input; interactive keeps all.
func TestCodexHeadlessDropsLeafTools(t *testing.T) {
	interactive := codexHeadless(context.Background(), []string{"--model", "m"}, nil)
	for name, h := range map[string]*agent.Headless{"tools-off": {}, "tools": {Tools: true}} {
		got := codexHeadless(context.Background(), nil, h)
		for _, c := range []string{"features.multi_agent_v2.disable_native_tools=true", "tools.experimental_request_user_input.enabled=false",
			`features.multi_agent_v2.root_agent_usage_hint_text=""`, `features.multi_agent_v2.multi_agent_mode_hint_text=""`} {
			if !slices.Contains(got, c) || slices.Contains(interactive, c) {
				t.Errorf("%s: %s: headless has it = %v, interactive has it = %v; want true, false", name, c, slices.Contains(got, c), slices.Contains(interactive, c))
			}
		}
	}
}
