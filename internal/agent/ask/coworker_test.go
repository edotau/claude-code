package ask

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// confine sets this process's confinement to provider[:profile] as ApplyCoworker exports it; "" is none.
func confine(t *testing.T, v string) {
	provider, profile, _ := strings.Cut(v, ":")
	t.Setenv(agentconfig.OverlayVar, agentconfig.InlineOverlay(provider, profile))
}

// ConfineLead fills an untyped lead from a non-Databricks confinement; a typed profile:model passes through untouched.
func TestConfineLeadFillsNonDatabricksLead(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{`+acmeProvider+`}`)
	cases := []struct {
		coworker, agent, provider, model string
		wantProfile, wantProvider        string
	}{
		{"openai:acme", "codex", "", "", "acme", ""},
		{"openai:acme", "codex", "openai", "", "acme", ""}, // a typed --provider means the pinned profile
		{"openai:acme", "codex", "", "acme:m", "", ""},     // a typed profile:model is never rewritten
		{"openai", "codex", "", "", "", "openai"},
		{"openai", "codex", "", "acme:m", "", ""},
		{"anthropic", "claude", "", "", "ultra", ""}, // claude reads a profile: its own CLI-login row
	}
	for _, c := range cases {
		confine(t, c.coworker)
		profile, provider := router.ConfineLead(c.agent, "", c.provider, c.model)
		if profile != c.wantProfile || provider != c.wantProvider {
			t.Errorf("%s %s --provider %q --model %q: ConfineLead = (%q, %q), want (%q, %q)",
				c.coworker, c.agent, c.provider, c.model, profile, provider, c.wantProfile, c.wantProvider)
		}
	}
}

// confineLine is a batch line's only fence: an off-route profile or an agent with no account is refused.
func TestConfineLineRefusesOffRouteBatchLine(t *testing.T) {
	vaulttest.Use(t)
	confine(t, "databricks:dev")
	if _, err := confineLine("claude", agent.RunOptions{Provider: "qa"}); err == nil ||
		!strings.Contains(err.Error(), "--profile qa: outside overlay databricks:dev") {
		t.Errorf("off-workspace line: err = %v, want --profile qa: outside overlay databricks:dev", err)
	}
	confine(t, "openai")
	if _, err := confineLine("copilot", agent.RunOptions{}); err == nil || !strings.Contains(err.Error(), "copilot has no openai account") {
		t.Errorf("copilot line under openai: err = %v, want copilot has no openai account", err)
	}
	got, err := confineLine("codex", agent.RunOptions{Prompt: "p"})
	reg, lerr := providers.Load()
	if err != nil || lerr != nil || got.Provider != "" || !slices.Contains(router.OwnProfiles(reg, "codex", "openai"), got.Lead) {
		t.Errorf("clean codex line under openai = lead %q provider %q, %v (%v); want codex's openai row as lead",
			got.Lead, got.Provider, err, lerr)
	}
}

// TestPickArm: fewest live wins; a tie goes to the least recent pick, then the name; an unusable workspace is skipped.
func TestPickArm(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	cases := []struct {
		name     string
		live     map[string]int
		picked   map[string]time.Time
		unusable []string
		want     string
		wantLive int
	}{
		{"fewest live", map[string]int{"dev": 3, "prod": 2, "qa": 1}, nil, nil, "qa", 1},
		{"tie: least recent pick", map[string]int{"dev": 1, "prod": 1, "qa": 1},
			map[string]time.Time{"dev": ago(time.Minute), "prod": ago(time.Hour), "qa": ago(2 * time.Minute)}, nil, "prod", 1},
		{"tie: never picked beats picked", nil, map[string]time.Time{"dev": ago(time.Hour), "prod": ago(time.Hour)}, nil, "qa", 0},
		{"tie: name order", nil, nil, nil, "dev", 0},
		{"no usable credential is skipped", map[string]int{"prod": 9, "qa": 5}, nil, []string{"dev"}, "qa", 5},
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"openai": {Name: "openai"}}}
	for _, n := range []string{"dev", "prod", "qa"} {
		reg.Providers[n] = &providers.Provider{Name: n, Preset: providers.PresetDatabricks}
	}
	origFindings, origCooling, origLive := armFindings, armCooling, armLive
	t.Cleanup(func() { armFindings, armCooling, armLive = origFindings, origCooling, origLive })
	armCooling = func(*providers.Provider, time.Time) bool { return false }
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			if err := writeArmPicks(armPicks{Schema: armPicksSchema, Picked: c.picked}); err != nil {
				t.Fatal(err)
			}
			armFindings = noCredential(c.unusable)
			armLive = func([]*providers.Provider) map[string]int { return c.live }
			got, live, skipped, err := pickArm(reg, "", "", now)
			if err != nil || got != c.want || live != c.wantLive || len(skipped) != len(c.unusable) {
				t.Fatalf("pickArm = %q, %d live, %d skipped, %v; want %q, %d live, %d skipped",
					got, live, len(skipped), err, c.want, c.wantLive, len(c.unusable))
			}
			if at := readArmPicks().Picked[got]; !at.Equal(now) {
				t.Fatalf("pick of %s stamped %v, want %v: the next tie would go to it again", got, at, now)
			}
		})
	}
	armFindings = noCredential([]string{"dev", "prod", "qa"})
	if got, _, _, err := pickArm(reg, "", "", now); err == nil {
		t.Fatalf("pickArm with no usable workspace = %q, want an error", got)
	}
}

// pickArm outside tried never returns a tried workspace, and none left is an error.
func TestPickArmExcludesTried(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	for _, n := range []string{"dev", "prod", "qa"} {
		reg.Providers[n] = &providers.Provider{Name: n, Preset: providers.PresetDatabricks}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	origFindings, origCooling, origLive := armFindings, armCooling, armLive
	t.Cleanup(func() { armFindings, armCooling, armLive = origFindings, origCooling, origLive })
	armFindings = noCredential(nil)
	armCooling = func(*providers.Provider, time.Time) bool { return false }
	armLive = func([]*providers.Provider) map[string]int { return nil }
	var tried []string
	for _, want := range []string{"dev", "prod", "qa"} {
		got, _, _, err := pickArm(reg, "", "", time.Now(), tried...)
		if err != nil || got != want {
			t.Fatalf("pickArm outside %v = %q, %v; want %q", tried, got, err, want)
		}
		tried = append(tried, got)
	}
	if got, _, _, err := pickArm(reg, "", "", time.Now(), tried...); err == nil {
		t.Fatalf("pickArm with every workspace tried = %q, want an error", got)
	}
}

// BenchmarkPickArmExcluding is the hot path a 429 adds: one pick outside the workspaces already tried.
func BenchmarkPickArmExcluding(b *testing.B) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	for _, n := range []string{"dev", "prod", "qa"} {
		reg.Providers[n] = &providers.Provider{Name: n, Preset: providers.PresetDatabricks}
	}
	b.Setenv("CLAUDE_CONFIG_DIR", b.TempDir())
	origFindings, origCooling, origLive := armFindings, armCooling, armLive
	b.Cleanup(func() { armFindings, armCooling, armLive = origFindings, origCooling, origLive })
	armFindings = noCredential(nil)
	armCooling = func(*providers.Provider, time.Time) bool { return false }
	armLive = func([]*providers.Provider) map[string]int { return nil }
	for b.Loop() {
		if _, _, _, err := pickArm(reg, "", "", time.Now(), "dev"); err != nil {
			b.Fatal(err)
		}
	}
}

// noCredential is an armFindings stub failing the credential check of each workspace in unusable.
func noCredential(unusable []string) func(*providers.Provider, string, string, time.Time) []agent.Finding {
	return func(p *providers.Provider, _, _ string, _ time.Time) []agent.Finding {
		if slices.Contains(unusable, p.Name) {
			return []agent.Finding{{Check: "credential", Profile: p.Name, Detail: "no stored credential for " + p.Name}}
		}
		return nil
	}
}

// TestArmFlagRefusals: each flag that would take a dispatch off overlay databricks:<p> exits 2 naming the flag.
func TestArmFlagRefusals(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{`+
		`"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"key","env":"DEV_WS_TOKEN"}},`+
		`"gw":{"preset":"databricks","host":"https://gw.cloud.databricks.com","auth":{"type":"pat"},"options":{"wire":"ai-gateway"}}}}}`)
	t.Setenv("DEV_WS_TOKEN", "SENTINEL-dev") // dev passes the workspace preflight, so each row reaches its flag fence
	if err := router.SaveServed(router.Served{Profile: "dev", FetchedAt: time.Now(), Models: []router.ServedModel{
		{ID: "databricks-claude-sonnet-5-5", Ready: "READY"}}}); err != nil { // claude's model fence reads the listing
		t.Fatal(err)
	}
	cases := []struct {
		inherited, want string
		args            []string
	}{
		{"", "overlay profile: no such profile", []string{"--overlay", `{"provider":"databricks","profile":"nope"}`}},
		{"", "overlay profile: AI Gateway row", []string{"--overlay", `{"provider":"databricks","profile":"gw"}`}},
		{"", "overlay profile: not a Databricks profile", []string{"--overlay", `{"provider":"databricks","profile":"chatgpt"}`}},
		{"", "overlay provider databricks needs a profile", []string{"--overlay", `{"provider":"databricks"}`}},
		{"", "--profile chatgpt: outside", []string{"--overlay", `{"provider":"databricks","profile":"dev"}`, "--profile", "chatgpt"}},
		{"", "--provider openai: outside", []string{"--overlay", `{"provider":"databricks","profile":"dev"}`, "--provider", "openai"}},
		{"", "--model: its profile is outside", []string{"--overlay", `{"provider":"databricks","profile":"dev"}`, "--model", "gw:m"}},
		{"", "--direct: refused", []string{"--overlay", `{"provider":"databricks","profile":"dev"}`, "--direct"}},
		{"", "--model: not served to claude", // external agents' models are the chain's preflight
			[]string{"--overlay", `{"provider":"databricks","profile":"dev"}`, "--agent", "claude", "--model", "gpt-nonexistent-9"}},
		{"databricks:dev", "cannot leave databricks:dev", []string{"--overlay", `{"provider":"openai"}`}},
		{"openai", "cannot leave openai", []string{"--overlay", `{"provider":"databricks","profile":"dev"}`}},
		{"openai", "--profile dev: outside overlay openai", []string{"--profile", "dev"}},
		{"anthropic", "codex has no anthropic account", nil},
		{"", "overlay profile: not a profile of openai", []string{"--overlay", `{"provider":"openai","profile":"dev"}`}},
		{"", "provider: unknown (anthropic|databricks|github|google|openai)", []string{"--overlay", `{"provider":"first-party"}`}},
		{"databricks:dev", "--profile chatgpt: outside", []string{"--profile", "chatgpt"}},
	}
	for _, tc := range cases {
		confine(t, tc.inherited) // ApplyCoworker exports into this process: restore after each case
		t.Setenv(providers.EnvSessionProfile, "")
		args := append([]string{"--agent", "codex", "--fallback", "none"}, append(tc.args, "go")...)
		code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd(args) })
		if code != 2 || !strings.Contains(stderr, tc.want) {
			t.Errorf("%v (inherited %q): exit %d stderr %q, want 2 with %q", tc.args, tc.inherited, code, stderr, tc.want)
		}
		if line := strings.TrimPrefix(strings.TrimSpace(stderr), "claude-code: ask: "); len(line) > 80 {
			t.Errorf("%v: message is %d columns, want at most 80: %q", tc.args, len(line), line)
		}
	}
}

// armPlanted are parent-env credentials no Databricks hop's child may hold (GH_TOKEN is kept for git and gh: I10).
var armPlanted = map[string]string{"ANTHROPIC_API_KEY": "SENTINEL-anthropic", "OPENAI_API_KEY": "SENTINEL-openai",
	"QA_WS_TOKEN": "SENTINEL-qa-ws", "DEV_WS_TOKEN": "SENTINEL-dev-ws", "COPILOT_GITHUB_TOKEN": "SENTINEL-gh"}

// armLogins are own-login credentials planted in the parent: a Databricks hop holds none, a first-party hop its own.
var armLogins = map[string]string{"OPENCODE_API_KEY": "opencode", "COPILOT_SDK_AUTH_TOKEN": "copilot",
	"GITHUB_TOKEN": "copilot", "CLAUDE_CODE_OAUTH_TOKEN": "claude"}

// armDevHop is what each agent's databricks:dev child must hold: its key variable and where its /p/dev route shows.
var armDevHop = map[string]struct{ bin, key, route string }{
	"claude":   {"claude", "HARNESS_PROXY_API_KEY", "ANTHROPIC_BASE_URL=http://127.0.0.1:9999/p/dev"},
	"codex":    {"codex", "HARNESS_CODEX_API_KEY", `base_url = "http://127.0.0.1:9999/p/dev/responses"`},
	"gemini":   {"agy", "AGY_LLM_GATEWAY_API_KEY", "AGY_LLM_GATEWAY_URL=http://127.0.0.1:9999/p/dev/gemini"},
	"opencode": {"opencode", "HARNESS_OPENCODE_API_KEY", `"baseURL":"http://127.0.0.1:9999/p/dev`},
	"copilot":  {"copilot", "COPILOT_PROVIDER_BEARER_TOKEN", "COPILOT_PROVIDER_BASE_URL=http://127.0.0.1:9999/p/dev"},
}

// armChildEnvFixture: dev/qa rows, planted env, a stubbed mint, fakes that dump argv+env to <dir>/<seq>-<bin> and 402.
func armChildEnvFixture(t *testing.T) string {
	t.Helper()
	skipWithoutSh(t)
	cliEnv(t)
	dump := t.TempDir()
	for _, h := range armDevHop {
		pathstest.FakeVendor(t, h.bin, `case "$1 $2" in login*) echo "Logged in using ChatGPT"; exit 0;; `+
			`--version*|*--help*|debug*) echo 1.0.0; exit 0;; esac`+"\n"+`n=$(ls `+dump+` | wc -l | tr -d ' ')`+"\n"+
			`{ echo "ARGV $*"; env; } > `+dump+`/$n-`+h.bin+"\n"+
			`echo '402 Payment Required: monthly quota exceeded' >&2; exit 1`)
	}
	row := func(name string) string {
		return `"` + name + `":{"preset":"databricks","host":"https://` + name + `.cloud.databricks.com",` +
			`"auth":{"type":"key","env":"` + strings.ToUpper(name) + `_WS_TOKEN"}}`
	}
	gh := `"github":{"personal":{"protocol":"openai","base_url":"https://api.githubcopilot.com","login":"octo",` +
		`"auth":{"type":"cli","login":"copilot"},"models":{"sonnet":"auto"}}}`
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{`+row("dev")+`,`+row("qa")+`},`+gh+`}}`)
	for k, v := range armPlanted {
		t.Setenv(k, v)
	}
	for k := range armLogins {
		t.Setenv(k, "SENTINEL-login")
	}
	t.Setenv("GH_TOKEN", "SENTINEL-gh")
	t.Setenv(providers.EnvSessionProfile, "qa")
	t.Setenv(agentconfig.OverlayVar, "") // ApplyCoworker and carryOverlay export into this process: restore after the run
	armCopilotLogin(t)
	orig := agent.MintSession
	agent.MintSession = func(_ context.Context, p []string, _ int) (string, error) {
		return "cct_ARM_" + strings.Join(p, ","), nil
	}
	t.Cleanup(func() { agent.MintSession = orig })
	return dump
}

// armCopilotLogin pins copilot to a gh login "octo" with verified credits, gh itself a fake that prints a token.
func armCopilotLogin(t *testing.T) {
	t.Helper()
	skipWithoutSh(t)
	if err := vault.SetAgentRecord("copilot", vault.AgentRecord{Profile: "personal"}); err != nil {
		t.Fatal(err)
	}
	gh, bin := t.TempDir(), t.TempDir()
	hosts := "github.com:\n    users:\n        octo:\n    user: octo\n"
	if err := os.WriteFile(filepath.Join(gh, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho gho-own-login\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CONFIG_DIR", gh)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	orig := copilot.CopilotUsedPct
	copilot.CopilotUsedPct = func(context.Context, string) (float64, bool) { return 10, true }
	t.Cleanup(func() { copilot.CopilotUsedPct = orig })
}

// armDumps reads the fake vendors' dumps in launch order: the binary and its argv line plus env entries.
func armDumps(t *testing.T, dir string) (bins []string, envs [][]string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(ents, func(a, b os.DirEntry) int { return seqOf(a.Name()) - seqOf(b.Name()) })
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		_, bin, _ := strings.Cut(e.Name(), "-")
		bins, envs = append(bins, bin), append(envs, strings.Split(string(b), "\n"))
	}
	return bins, envs
}

func seqOf(name string) int { n, _ := strconv.Atoi(strings.SplitN(name, "-", 2)[0]); return n }

// envValue is name's value in a dump; ok is false when name is absent.
func envValue(dump []string, name string) (string, bool) {
	for _, kv := range dump {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

// armDevDoc is the one-hop $COWORKER_OVERLAY a databricks:dev hop's child inherits: the confinement alone.
const armDevDoc = `{"provider":"databricks","profile":"dev"}`

// assertArmDevHop: the child is on dev's route, its key the dev session token, inside the coworker, leaking nothing.
func assertArmDevHop(t *testing.T, name, goos string, dump []string) {
	t.Helper()
	h := armDevHop[name]
	if name == "copilot" && goos != "darwin" {
		assertArmCopilotRegistryHop(t, dump)
		return
	}
	if !slices.ContainsFunc(dump, func(l string) bool { return strings.Contains(l, h.route) }) {
		t.Errorf("%s dev hop: no %s route", name, h.route)
	}
	if v, _ := envValue(dump, h.key); h.key != "" && v != "cct_ARM_dev" {
		t.Errorf("%s dev hop: %s is not the dev session token", name, h.key)
	}
	if v, _ := envValue(dump, agentconfig.OverlayVar); v != armDevDoc {
		t.Errorf("%s dev hop: %s is not %s", name, agentconfig.OverlayVar, armDevDoc)
	}
	assertArmNoLeak(t, name, dump)
}

// assertArmCopilotRegistryHop: off macOS the hop runs from the private home; route and token ride the SDK, not env.
func assertArmCopilotRegistryHop(t *testing.T, dump []string) {
	t.Helper()
	if v, _ := envValue(dump, "COPILOT_HOME"); !strings.HasSuffix(v, filepath.Join("copilot", "registry-home")) {
		t.Errorf("copilot dev hop: COPILOT_HOME = %q, want the private registry home", v)
	}
	for _, kv := range dump {
		if strings.HasPrefix(kv, "COPILOT_PROVIDER_") {
			t.Errorf("copilot dev hop: registry child inherited the BYOK block %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
	if v, _ := envValue(dump, agentconfig.OverlayVar); v != armDevDoc {
		t.Errorf("copilot dev hop: %s is not %s", agentconfig.OverlayVar, armDevDoc)
	}
	assertArmNoLeak(t, "copilot", dump)
}

// assertArmNoLeak: no planted credential or router secret.
func assertArmNoLeak(t *testing.T, name string, dump []string) {
	t.Helper()
	for k := range armPlanted {
		if _, ok := envValue(dump, k); ok {
			t.Errorf("%s dev hop: inherited %s", name, k)
		}
	}
	assertArmNoLogin(t, name, dump)
	if _, ok := envValue(dump, "GH_TOKEN"); !ok && name != "copilot" {
		t.Errorf("%s dev hop: lost GH_TOKEN, which git and gh need", name)
	}
	assertArmNoSecret(t, name, dump)
}

// assertArmNoSecret: no entry holds the router secret or names another profile.
func assertArmNoSecret(t *testing.T, name string, dump []string) {
	t.Helper()
	for _, kv := range dump {
		if k, v, _ := strings.Cut(kv, "="); v == "rtr-secret-test" || (k == providers.EnvSessionProfile && v != "dev") {
			t.Errorf("%s dev hop: %s holds the router secret or another profile", name, k)
		}
	}
}

// assertArmNoLogin: a dev hop inherits no agent's own login.
func assertArmNoLogin(t *testing.T, name string, dump []string) {
	t.Helper()
	for k := range armLogins {
		if _, ok := envValue(dump, k); ok {
			t.Errorf("%s dev hop: inherited own-login %s", name, k)
		}
	}
}

// armDatabricksEntry reports an env entry that carries a Databricks credential, session token or route.
func armDatabricksEntry(k, v, kv string) bool {
	return k == "DEV_WS_TOKEN" || k == "QA_WS_TOKEN" || strings.HasPrefix(k, "DATABRICKS_") ||
		strings.Contains(v, "cct_ARM") || strings.Contains(kv, "/p/dev") || strings.Contains(kv, "databricks.com")
}

// assertArmOwnLogin: an own-login hop holds no Databricks credential, session token or /p/ route.
func assertArmOwnLogin(t *testing.T, label string, dump []string) {
	t.Helper()
	for _, kv := range dump {
		k, v, _ := strings.Cut(kv, "=")
		if armDatabricksEntry(k, v, kv) {
			t.Errorf("%s own-login hop: %s carries a Databricks credential or route", label, k)
		}
		if owner, ok := armLogins[k]; ok && owner != label {
			t.Errorf("%s own-login hop: inherited %s's own login %s", label, owner, k)
		}
	}
	if _, ok := envValue(dump, "OPENCODE_API_KEY"); label == "opencode" && !ok {
		t.Errorf("opencode own-login hop: lost its own login OPENCODE_API_KEY")
	}
}

// In databricks:dev each dev hop holds only dev's route and session token; after a dev 402 first-party hops hold none.
func TestArmChildEnvPerAgent(t *testing.T) {
	for name := range armDevHop {
		for _, goos := range []string{"linux", "darwin"} { // copilot's hop differs per OS; the rest ignore it
			if name != "copilot" && goos == "darwin" {
				continue
			}
			t.Run(name+"/"+goos, func(t *testing.T) { checkArmChildEnv(t, name, goos) })
		}
	}
}

// checkArmChildEnv runs one agent's databricks:dev ask with copilot's OS pinned to goos and checks every hop's env.
func checkArmChildEnv(t *testing.T, name, goos string) {
	t.Helper()
	dump := armChildEnvFixture(t)
	t.Cleanup(copilot.StubGOOS(goos))
	t.Cleanup(copilot.StubRegistry("cct_ARM_dev", "databricks-claude-sonnet-5-5", "databricks-gpt-6-1-sol")) // no live discovery
	code, _, stderr := runCLI(t, "ask", "--agent", name, "--overlay", `{"provider":"databricks","profile":"dev"}`, "--fallback", "all", "go")
	bins, envs := armDumps(t, dump)
	if code == 0 || len(bins) == 0 || bins[0] != armDevHop[name].bin {
		t.Fatalf("exit %d launches %v, want %s's dev hop first\n%s", code, bins, name, stderr)
	}
	assertArmDevHop(t, name, goos, envs[0])
	for i := 1; i < len(bins); i++ {
		assertArmOwnLogin(t, bins[i], envs[i])
	}
	want := []string{armDevHop[name].bin, "claude"} // the agent's own-login pin, then claude
	if name == "claude" || name == "opencode" {     // claude has no pin hop; the fixture pins opencode outside dev
		want = []string{"claude"}
	}
	if !slices.Equal(bins[1:], want) {
		t.Errorf("launches after the dev hop %v, want the pin then claude %v\n%s", bins[1:], want, stderr)
	}
}

// Under --fallback none a databricks:dev ask is exactly one launch: no own-login hop runs inside the arm env.
func TestArmChildEnvFallbackNoneOneHop(t *testing.T) {
	dump := armChildEnvFixture(t)
	code, _, stderr := runCLI(t, "ask", "--agent", "codex", "--overlay", `{"provider":"databricks","profile":"dev"}`, "--fallback", "none", "go")
	bins, envs := armDumps(t, dump)
	if code == 0 || !slices.Equal(bins, []string{"codex"}) {
		t.Fatalf("exit %d launches %v, want only codex's dev hop\n%s", code, bins, stderr)
	}
	assertArmDevHop(t, "codex", "linux", envs[0])
}

// --native is refused under any confinement: opencode's own default login is outside every overlay provider.
func TestCheckNativeRefusedUnderConfinement(t *testing.T) {
	dump := armChildEnvFixture(t)
	for _, c := range []struct{ agent, overlay, want string }{
		{"opencode", `{"provider":"databricks","profile":"dev"}`, "--native: outside overlay databricks:dev"},
		{"codex", `{"provider":"openai"}`, "--native: outside overlay openai"}, // opencode has no openai account to pass canUse
	} {
		t.Setenv(agentconfig.OverlayVar, "")
		t.Setenv(providers.EnvSessionProfile, "qa")
		code, _, stderr := runCLI(t, "ask", "--agent", c.agent, "--native", "--overlay", c.overlay, "go")
		if code != 2 || !strings.Contains(stderr, c.want) {
			t.Errorf("%s: exit %d stderr %q, want 2 with %q", c.overlay, code, stderr, c.want)
		}
	}
	if bins, _ := armDumps(t, dump); len(bins) != 0 || len(agent.RecentRecords(1)) != 0 {
		t.Fatalf("refused --native launched %v or saved a record", bins)
	}
}
