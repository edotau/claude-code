package agent

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/agent/retry"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A preset provider whose credential command fails must fail the host guard, not skip it.
func TestGuardRouteHostFailsOnCredentialError(t *testing.T) {
	vaulttest.UseKeyring(t)
	p := &providers.Provider{Name: "dbx", Preset: providers.PresetDatabricks, Auth: providers.Auth{Type: "key", Command: "false"}}
	if err := guardRouteHost(context.Background(), p); err == nil {
		t.Fatal("guardRouteHost = nil, want the credential error")
	}
}

// An isolated hop fails up front when the registry cannot load, since the foreign-credential scrub would be skipped.
func TestCheckIsolationFailsWhenRegistryUnreadable(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{not json`)
	if err := checkIsolation(withIsolation(context.Background())); err == nil {
		t.Fatal("checkIsolation = nil, want the registry load error")
	}
	if err := checkIsolation(context.Background()); err != nil {
		t.Fatalf("non-isolated ctx: err = %v, want nil", err)
	}
}

// An isolated own-login hop (no provider) must also fail up front on an unreadable registry, not run unscrubbed.
func TestGuardAttemptOwnLoginIsolatedFailsWhenRegistryUnreadable(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{not json`)
	r := &fakeRunner{}
	_, gerr := guardAttempt(withIsolation(context.Background()), r, RunOptions{})
	if gerr == nil || gerr.Kind != retry.KindFatal {
		t.Fatalf("isolated guardAttempt = %v, want KindFatal", gerr)
	}
	if _, gerr := guardAttempt(context.Background(), r, RunOptions{}); gerr != nil {
		t.Fatalf("non-isolated guardAttempt = %v, want nil", gerr)
	}
}

// hmProfile overlays a Databricks profile "hm" on host adb-1 whose credential comes from auth.command cmd.
func hmProfile(t *testing.T, cmd string) { hmNamed(t, "hm", cmd) }

// hmNamed is hmProfile under name; command credentials are cached per name for the process, so reruns need fresh ones.
func hmNamed(t *testing.T, name, cmd string) {
	t.Helper()
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"`+name+`":{"preset":"databricks",`+
		`"host":"https://adb-1.azuredatabricks.net","auth":{"type":"key","command":`+fmt.Sprintf("%q", cmd)+`},`+
		`"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5","haiku":"databricks-claude-haiku-4-5"}}}}}`)
}

// A routed provider:model request on a vendor agent is guarded as that provider, not skipped as an own login.
func TestGuardProviderResolvesModelPrefixForVendorAgent(t *testing.T) {
	hmProfile(t, "true")
	p, err := guardProvider(&fakeRunner{name: "codex"}, RunOptions{Model: "hm:opus"})
	if err != nil || p == nil || p.Name != "hm" {
		t.Fatalf("guardProvider = %v, %v; want the hm profile", p, err)
	}
}

// armRegistry overlays workspaces dev and qa (DEV_TOKEN, QA_TOKEN; devExtra extends dev) and arms databricks:dev.
func armRegistry(t *testing.T, devExtra string) {
	t.Helper()
	vaulttest.UseKeyring(t)
	row := func(name, env, extra string) string {
		return `"` + name + `":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net",` +
			`"auth":{"type":"key","env":"` + env + `"},"models":{"opus":"databricks-claude-opus-5-5"}` + extra + `}`
	}
	vaulttest.Put(t, vault.Overlay, "providers",
		`{"providers":{"databricks":{`+row("dev", "DEV_TOKEN", devExtra)+`,`+row("qa", "QA_TOKEN", "")+`}}}`)
	t.Setenv("DEV_TOKEN", "dapi-dev")
	t.Setenv("QA_TOKEN", "dapi-qa")
	t.Setenv(agentconfig.OverlayVar, `{"provider":"databricks","profile":"dev"}`)
}

// Inside databricks:dev a hop runs only on dev or the agent's own login, isolated; anything else is fatal up front.
func TestArmGuard(t *testing.T) {
	cases := []struct {
		name, devExtra, prof, fatal string
		unarmed                     bool
		creds                       []string // the credential names the child keeps on a pass
	}{
		{name: "another workspace", prof: "qa", fatal: "profile qa is outside databricks:dev"},
		{name: "another agent's login", prof: "chatgpt", fatal: "profile chatgpt is outside databricks:dev"},
		{name: "AI Gateway dev", devExtra: `,"options":{"wire":"ai-gateway"}`, prof: "dev", fatal: "AI Gateway wire"},
		{name: "dev", prof: "dev"}, // proxied on the session token: no workspace token
		{name: "own login", prof: "ultra"},
		{name: "unarmed qa", prof: "qa", unarmed: true, creds: []string{"DEV_TOKEN", "QA_TOKEN"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			armRegistry(t, c.devExtra)
			if c.unarmed {
				t.Setenv(agentconfig.OverlayVar, "")
			}
			armGuardCase(t, c.prof, c.fatal, c.creds)
		})
	}
}

// armGuardCase runs claude's guard on prof: fatal names the refusal, else creds are the credential names kept.
func armGuardCase(t *testing.T, prof, fatal string, creds []string) {
	t.Helper()
	ctx, gerr := guardAttempt(context.Background(), &fakeRunner{name: "claude"}, RunOptions{Provider: prof, Model: "opus"})
	if fatal != "" {
		if gerr == nil || gerr.Kind != retry.KindFatal || !strings.Contains(gerr.Error(), fatal) {
			t.Errorf("guardAttempt = %v, want KindFatal %q", gerr, fatal)
		}
		return
	}
	if gerr != nil {
		t.Fatalf("guardAttempt = %v, want a pass", gerr)
	}
	env := EnvSpec{Mode: EnvPrescrubbed, Base: []string{"DEV_TOKEN=dapi-dev", "QA_TOKEN=dapi-qa"}}.Environ(ctx)
	got := slices.DeleteFunc(envNames(env), func(n string) bool { return !strings.HasSuffix(n, "_TOKEN") })
	if !slices.Equal(got, creds) {
		t.Errorf("child credential names = %v, want %v", got, creds)
	}
}

// Inside databricks:dev a claude hop runs on its candidate's "<profile>:" row, which the guard then checks.
func TestArmClaudeHopRunsOnItsProfile(t *testing.T) {
	armRegistry(t, "")
	claude := &fakeRunner{name: "claude"}
	for prof, fatal := range map[string]bool{"ultra": false, "qa": true} {
		req := dispatchRequest(RunOptions{Provider: "dev"}, Dispatch{Agent: claude, Model: prof + ":sonnet"}, false)
		if req.Provider != prof || req.Model != "sonnet" {
			t.Errorf("%s hop: provider %q model %q, want %q and sonnet", prof, req.Provider, req.Model, prof)
		}
		if _, gerr := guardAttempt(context.Background(), claude, req); (gerr != nil) != fatal {
			t.Errorf("%s hop: guardAttempt = %v, want fatal %v", prof, gerr, fatal)
		}
	}
}

// Inside databricks:dev every hop the pick admits is one guardArm allows, for every wanted agent and pin shape.
func TestArmPickAdmitsOnlyGuardAllowedHops(t *testing.T) {
	cases := []struct {
		name string
		pins map[string]string
	}{
		{"pinned to the workspace", map[string]string{"codex": "dev", "copilot": "dev", "gemini": "dev", "opencode": "dev"}},
		{"pinned off the coworker", map[string]string{"codex": "openai", "copilot": "qa", "gemini": "qa", "opencode": "qa"}},
		{"pinned to own logins", map[string]string{"codex": "chatgpt", "copilot": "personal", "gemini": "antigravity",
			"opencode": "opencode-go"}},
	}
	configured := func(string, router.Cell) error { return nil }
	for _, c := range cases {
		for _, want := range agentid.All() {
			t.Run(c.name+"/"+want, func(t *testing.T) {
				armRegistry(t, "")
				for a, p := range c.pins {
					vaulttest.SetRecord(t, a, "profile", p)
				}
				chain, _, err := RouteChainFor(t.Context(), want, "sonnet", "", router.FallbackAll, false,
					router.PickOpts{Configured: configured})
				if err != nil || len(chain.Dispatches) == 0 {
					t.Fatalf("chain = %+v, %v; want admitted hops", chain, err)
				}
				for i, d := range chain.Dispatches {
					req := dispatchRequest(RunOptions{Prompt: "p"}, d, i == 0 && d.Agent.Name() == want)
					gp, err := guardProvider(d.Agent, req)
					if err == nil {
						err = guardArm("dev", d.Agent, gp)
					}
					if err != nil {
						t.Errorf("hop %d %s: admitted by the pick, refused by the guard: %v", i, hopTarget(d), err)
					}
				}
			})
		}
	}
}

// A frozen copilot target's gate note prints once through NoticeOut, however many attempts resolve the target.
func TestTargetCtxPrintsGateNoteOnce(t *testing.T) {
	var buf bytes.Buffer
	orig := NoticeOut
	NoticeOut = &buf
	t.Cleanup(func() { NoticeOut = orig })
	ctx := context.WithValue(context.Background(), frozenKey{}, frozenTarget{own: true, note: "gate moved copilot", once: &sync.Once{}})
	for range 2 {
		if _, _, err := SchemaFor("copilot").TargetCtx(ctx, RunOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(buf.String(), "gate moved copilot\n"); got != 1 {
		t.Errorf("NoticeOut = %q, want the gate note exactly once", buf.String())
	}
}
