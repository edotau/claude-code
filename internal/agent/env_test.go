package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/paths/pathstest"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/proxyteam"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/rtkwrap"
)

func TestRunValuesProjectsTrustedDispatch(t *testing.T) {
	setConfigDir(t)
	stale := proc.Values{proc.DispatchIDEnv: "stale", proc.DispatchDepthEnv: "0"}
	original := stale.Merge(nil)
	for i, id := range []string{"first", "second"} {
		cap := i + 3
		parent := Record{ID: id, Agent: "codex", Status: "running", Depth: i + 1, MaxDepth: &cap}
		fixtureRecord(t, parent)
		ctx := withDispatchID(t.Context(), id, parent.Depth)
		for _, input := range []struct {
			ctx    context.Context
			values proc.Values
		}{{ctx, nil}, {proc.WithValues(ctx, stale), nil}, {ctx, stale}} {
			childCtx, values := RunValues(input.ctx, input.values)
			want := []string{id, strconv.Itoa(parent.Depth)}
			if got := []string{proc.Value(childCtx, proc.DispatchIDEnv), values[proc.DispatchDepthEnv]}; !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: trusted identity lost (explicit=%v)", id, input.values != nil)
			}
			childCtx, _ = RunValues(childCtx, stale)
			child, err := (&Client{}).Record(childCtx, RunOptions{Agent: "codex", Values: stale}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := []any{child.Parent, child.Depth, *child.MaxDepth}, []any{id, parent.Depth + 1, cap}; !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: child parent=%s depth=%d cap=%d", id, child.Parent, child.Depth, *child.MaxDepth)
			}
		}
	}
	if !reflect.DeepEqual(stale, original) {
		t.Fatal("trusted projection mutated supplied values")
	}
}

// Given a launching workspace in the env, Then every child env carries it exactly once, explicit Base included.
func TestEnvironPassesMemoryOrigin(t *testing.T) {
	t.Setenv(memory.OriginEnv, "/work/space")
	want := memory.OriginEnv + "=/work/space"
	for _, s := range []EnvSpec{{Mode: EnvClaude}, {Mode: EnvAgent}, {Mode: EnvClaude, Base: []string{"PATH=/bin"}}} {
		env := s.Environ(context.Background())
		if n := len(slices.DeleteFunc(slices.Clone(env), func(e string) bool { return e != want })); n != 1 {
			t.Errorf("%+v: %s present %d times, want 1", s.Mode, want, n)
		}
	}
}

// Given a scrubbed name inherited and the same name set by the launch, Then the child gets the launch's value only.
func TestEnvironLaunchSetSurvivesScrub(t *testing.T) {
	const name = "AGY_LLM_GATEWAY_URL"
	t.Setenv(name, "http://inherited.invalid")
	count := func(env []string, prefix string) int {
		return len(slices.DeleteFunc(slices.Clone(env), func(e string) bool { return !strings.HasPrefix(e, prefix) }))
	}
	inherited := EnvSpec{Mode: EnvAgent}.Environ(context.Background())
	if n := count(inherited, name+"="); n != 0 {
		t.Errorf("inherited %s reached the child %d times, want scrubbed", name, n)
	}
	set := EnvSpec{Mode: EnvAgent, Set: []string{name + "=http://launch.invalid"}}.Environ(context.Background())
	if n := count(set, name+"="); n != 1 || !slices.Contains(set, name+"=http://launch.invalid") {
		t.Errorf("launch-set %s: want exactly the launch value, got %d entries", name, n)
	}
	// negative control: a prescrubbed base keeps the name, so only the dropper can stop a stale duplicate
	pre := EnvSpec{Mode: EnvPrescrubbed, Base: []string{name + "=http://stale.invalid"}, Set: []string{name + "=http://launch.invalid"}}.
		Environ(context.Background())
	if n := count(pre, name+"="); n != 1 || !slices.Contains(pre, name+"=http://launch.invalid") {
		t.Errorf("prescrubbed base + Set %s: want only the launch value, got %d entries", name, n)
	}
}

// A dispatch child gets the carried env as its overlay copy only; a plain launch keeps the inherited names.
func TestEnvironDispatchChildTakesCarriedFromOverlay(t *testing.T) {
	for _, n := range proc.DispatchCarried {
		t.Setenv(n, "")
	}
	t.Setenv("AGENT_ERROR_FALLBACK", "none")
	t.Setenv("CODEMODE", "on")
	t.Setenv("CODEX_MODEL", "x")
	t.Setenv("HARNESS_KEEP_ENV", "FOO")
	t.Setenv("HARNESS_DISPATCH_MAX_CHILDREN", "3")
	child := EnvSpec{Mode: EnvAgent, Unset: []string{route.OverlayVar}}.Environ(withDispatchID(context.Background(), "d1", 0))
	for _, n := range []string{"AGENT_ERROR_FALLBACK", "CODEX_MODEL", "HARNESS_KEEP_ENV", "HARNESS_DISPATCH_MAX_CHILDREN"} {
		if v := proc.LastEnv(child, n); v != "" {
			t.Errorf("dispatch child inherited %s", n)
		}
	}
	doc := proc.LastEnv(child, "COWORKER_OVERLAY")
	for _, want := range []string{`"fallback":"none"`, `"codemode":"on"`, `"maxChildren":3`} {
		if !strings.Contains(doc, want) {
			t.Errorf("overlay copy %s lacks %s", doc, want)
		}
	}
	if strings.Contains(doc, "model") {
		t.Errorf("overlay copy %s carries a model", doc)
	}
	if got := proc.LastEnv(child, "CODEMODE"); got != "on" {
		t.Errorf("child CODEMODE = %q, want on from the overlay", got)
	}
	carried := proc.WithValues(t.Context(), proc.Values{proc.DispatchIDEnv: "p1", "CODEMODE": "on"}) // a carried child: id in values only
	if got := proc.LastEnv(EnvSpec{Mode: EnvClaude}.Environ(carried), "CODEMODE"); got != "on" {
		t.Errorf("carried child CODEMODE = %q, want on", got)
	}
	if got := proc.LastEnv(EnvSpec{Mode: EnvAgent}.Environ(context.Background()), "CODEX_MODEL"); got != "x" {
		t.Errorf("a non-dispatch launch lost CODEX_MODEL: %q", got)
	}
}

// A provider-only unit keeps its caps in the child's copy; an unreadable parent doc leaves a fence nothing passes.
func TestChildOverlayProviderOnlyAndUnreadable(t *testing.T) {
	ctx := proc.WithValues(t.Context(), proc.Values{route.OverlayVar: `{"maxChildren":2}`})
	if o := ChildOverlay(ctx, "openai", ""); o.Provider != "openai" || o.MaxChildren == nil || *o.MaxChildren != 2 || o.Fence != nil {
		t.Errorf("provider-only copy %+v, want openai with maxChildren 2 and no fence", o)
	}
	bad := proc.WithValues(t.Context(), proc.Values{route.OverlayVar: `{"maxChildren":2`}) // malformed: a stored unknown key now loads, dropped
	o := ChildOverlay(bad, "databricks", "dev")
	if _, err := agentconfig.LoadOverlayContext(t.Context(), childDoc(o), nil); err == nil || !strings.Contains(err.Error(), "outside the fence") {
		t.Errorf("unreadable parent: child doc %s (%v), want a fence the child refuses to open", childDoc(o), err)
	}
	stored := proc.WithValues(t.Context(), proc.Values{route.OverlayVar: `{"nope":1,"maxChildren":2}`})
	if o := ChildOverlay(stored, "openai", ""); o.Fence != nil || o.MaxChildren == nil || *o.MaxChildren != 2 {
		t.Errorf("stored unknown key: child %+v, want the known keys kept and no fence", o)
	}
}

// A fenced provider-only base refuses a unit on another provider, as Derive does for a full unit.
func TestDeriveUnitKeepsFencedProvider(t *testing.T) {
	base := agentconfig.Overlay{Provider: "openai", Fence: &route.Fence{}}
	if _, err := DeriveUnit(base, agentconfig.Overlay{Provider: "databricks", Profile: "dev"}, agentconfig.HopAppend); err == nil {
		t.Error("a fenced openai base accepted a databricks unit")
	}
	if _, err := DeriveUnit(base, agentconfig.Overlay{Provider: "databricks"}, agentconfig.HopAppend); err == nil {
		t.Error("a fenced openai base accepted a provider-only databricks unit")
	}
}

// An agent CLI child drops inherited credential-shaped names except its own login, HARNESS_KEEP_ENV and launch values.
func TestEnvironAgentDropsInheritedCredentials(t *testing.T) {
	inherited := []string{"PATH=/bin", "MISTRAL_API_KEY=x", "GITHUB_PERSONAL_ACCESS_TOKEN=x", "SOME_SERVICE_SECRET=x",
		"GH_TOKEN=x", "OPENCODE_API_KEY=x", "DATABRICKS_TOKEN=inherited"}
	rows := []struct {
		name    string
		spec    EnvSpec
		keepEnv string
		want    []string // credential-shaped names the child ends with
	}{
		{"codex", EnvSpec{Mode: EnvAgent, Agent: "codex"}, "", nil},
		{"copilot keeps its login", EnvSpec{Mode: EnvAgent, Agent: "copilot"}, "", []string{"GH_TOKEN"}},
		{"gemini", EnvSpec{Mode: EnvAgent, Agent: "gemini"}, "", nil},
		{"opencode keeps its login", EnvSpec{Mode: EnvAgent, Agent: "opencode"}, "", []string{"OPENCODE_API_KEY"}},
		{"no agent", EnvSpec{Mode: EnvAgent}, "", nil},
		{"claude is unchanged", EnvSpec{Mode: EnvClaude}, "", []string{"MISTRAL_API_KEY", "GITHUB_PERSONAL_ACCESS_TOKEN",
			"SOME_SERVICE_SECRET", "GH_TOKEN", "OPENCODE_API_KEY"}},
		{"HARNESS_KEEP_ENV", EnvSpec{Mode: EnvAgent, Agent: "codex"}, "MISTRAL_API_KEY", []string{"MISTRAL_API_KEY"}},
		{"launch value after the scrub", EnvSpec{Mode: EnvAgent, Agent: "codex", Set: []string{"DATABRICKS_TOKEN=launch"}}, "",
			[]string{"DATABRICKS_TOKEN"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Setenv("HARNESS_KEEP_ENV", r.keepEnv)
			r.spec.Base = inherited
			env := r.spec.Environ(context.Background())
			got := slices.DeleteFunc(envNames(env), func(n string) bool {
				return !strings.HasSuffix(n, "_KEY") && !strings.HasSuffix(n, "_TOKEN") && !strings.HasSuffix(n, "_SECRET")
			})
			if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(r.want))) {
				t.Errorf("credential-shaped names = %v, want %v", got, r.want)
			}
			if slices.Contains(env, "DATABRICKS_TOKEN=inherited") {
				t.Error("an inherited DATABRICKS_TOKEN reached the child")
			}
		})
	}
}

// injectedRows are credentials a launch injects; each must reach the child as the injected value only.
var injectedRows = []struct{ agent, name string }{
	{"codex", "DATABRICKS_TOKEN"}, {"gemini", "DATABRICKS_TOKEN"}, {"copilot", "COPILOT_GITHUB_TOKEN"},
	{"opencode", "OPENCODE_API_KEY"}, {"opencode", "HARNESS_OPENCODE_API_KEY"},
	{"codex", "OPENAI_API_KEY"}, {"gemini", "GEMINI_API_KEY"},
}

// injectedOnce is name's entries in env: how many, and the value the child would see (the last one wins).
func injectedOnce(env []string, name string) (n int, val string) {
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == name {
			n, val = n+1, v
		}
	}
	return n, val
}

// A credential the launch injects replaces a different inherited value of the same name, for every agent child.
func TestEnvironInjectedCredentialBeatsInherited(t *testing.T) {
	for _, r := range injectedRows {
		t.Run(r.agent+"/"+r.name, func(t *testing.T) {
			spec := EnvSpec{Mode: EnvAgent, Agent: r.agent, Base: []string{"PATH=/bin", r.name + "=inherited"},
				Set: []string{r.name + "=injected"}}
			if n, val := injectedOnce(spec.Environ(context.Background()), r.name); n != 1 || val != "injected" {
				t.Errorf("%s: %d entries, value %q; want one entry, injected", r.name, n, val)
			}
		})
	}
}

func TestChildDeadlineKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups and /bin/sh are unix-only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	old := childWaitDelay
	childWaitDelay = 20 * time.Second // only a group kill can end Run before this
	t.Cleanup(func() { childWaitDelay = old })
	cmd, tail := ChildCmd(ctx, "/bin/sh", []string{"-c", "sleep 30 & sleep 30"}, nil, "", "", nil, false)
	start := time.Now()
	err := ChildErr(ctx, "sh", cmd.Run(), tail)
	if time.Since(start) >= childWaitDelay || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("grandchild outlived the deadline: took %v, err %v", time.Since(start), err)
	}
}

// ChildCmd sets $PWD with cmd.Dir for a child reading $PWD; an empty dir leaves PWD unset so the child inherits cwd.
func TestChildCmdSetsPWD(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh")
	dir := t.TempDir()
	cmd, _ := ChildCmd(context.Background(), "/bin/sh", []string{"-c", "true"}, nil, dir, "", nil, false)
	if !slices.Contains(cmd.Env, "PWD="+dir) {
		t.Errorf("cmd.Env lacks PWD=%s", dir) // never print cmd.Env: it holds tokens
	}

	// dir == "" (no --workdir): ChildCmd must append nothing past the scrubbed environ, no PWD.
	t.Setenv(memory.OriginEnv, "/inherited")   // already inherited, so withMemoryOrigin adds nothing either
	t.Setenv("RTK_SUPPRESS_HOOK_WARNING", "1") // likewise for the rtk banner switch
	t.Setenv("RTK_TELEMETRY_DISABLED", "1")
	scrubbed, _ := proc.Scrub(os.Environ(), nil)
	cmd, _ = ChildCmd(context.Background(), "/bin/sh", []string{"-c", "true"}, nil, "", "", nil, false)
	if n, want := len(cmd.Env), len(scrubbed); n != want {
		t.Errorf("cmd.Env has %d entries, want %d (scrubbed os.Environ(), no PWD appended)", n, want)
	}
}

// A vendor CLI child never sees HARNESS_VAULT_KEY; the claude agent's own child keeps it since hooks read the vault.
func TestChildCmdScrubsVaultKeyForVendorOnly(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh")
	t.Setenv("HARNESS_VAULT_KEY", "secret-value")
	hasKey := func(env []string) bool {
		return slices.ContainsFunc(env, func(s string) bool { return strings.HasPrefix(s, "HARNESS_VAULT_KEY=") })
	}
	cmd, _ := ChildCmd(context.Background(), "/bin/sh", []string{"-c", "true"}, nil, "", "", nil, true)
	if hasKey(cmd.Env) {
		t.Error("vendor child env leaked HARNESS_VAULT_KEY")
	}
	cmd, _ = ChildCmd(context.Background(), "/bin/sh", []string{"-c", "true"}, nil, "", "", nil, false)
	if !hasKey(cmd.Env) {
		t.Error("claude's own child must keep HARNESS_VAULT_KEY")
	}
}

// TestLogSinkStdoutByteIdentical: a sink must not change what the caller's stdout receives.
func TestLogSinkStdoutByteIdentical(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake child is an sh script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake")
	script := "#!/bin/sh\nprintf 'alpha\\nbeta\\n'\necho oops >&2\nprintf 'gamma\\nno-newline'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context) []byte {
		t.Helper()
		var buf bytes.Buffer
		cmd, _ := ChildCmd(ctx, bin, nil, nil, "", "", nil, true)
		cmd.Stdout = wire.TeeWriter(ctx, &buf)
		if err := cmd.Run(); err != nil {
			t.Fatalf("run fake: %v", err)
		}
		return buf.Bytes()
	}
	plain := run(context.Background())
	logPath := filepath.Join(dir, "job.log")
	sink := wire.NewLogSink(logPath)
	teed := run(wire.WithLog(context.Background(), sink))
	sink.Close()

	if !bytes.Equal(plain, teed) {
		t.Errorf("stdout differs with a sink:\nplain %q\nteed  %q", plain, teed)
	}
	if want := "alpha\nbeta\ngamma\nno-newline"; string(plain) != want {
		t.Errorf("stdout = %q, want %q", plain, want)
	}
	log := pathstest.ReadFile(t, logPath)
	for _, l := range []string{"alpha\n", "beta\n", "gamma\n", "no-newline\n", "oops\n"} {
		if !strings.Contains(log, l) {
			t.Errorf("log %q missing %q", log, l)
		}
	}
}

func TestCredentialScrubRegistryFailure(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{not json`)
	if _, err := providers.Load(); err == nil {
		t.Fatal("fixture must make the provider registry unreadable")
	}
	env := []string{"PATH=/bin", "FOREIGN_KEY=key", "FOREIGN_TOKEN=token", "FOREIGN_SECRET=secret",
		"FOREIGN_PASSWORD=password", "FOREIGN_CREDENTIALS=credentials", "KEPT_TOKEN=kept", "OWN_KEY=own"}
	for _, own := range []bool{false, true} {
		want := []string{"PATH=/bin", "KEPT_TOKEN=kept"}
		ctx := withIsolation(context.Background())
		if own {
			ctx = withOwnProvider(ctx, &providers.Provider{Name: "mine", Auth: providers.Auth{Env: "OWN_KEY"}})
			want = append(want, "OWN_KEY=own")
		}
		if got := scrubIsolated(ctx, env, []string{"KEPT_TOKEN"}); !reflect.DeepEqual(got, want) {
			t.Errorf("own=%v: scrubbed env = %d, want %q", own, len(got), want)
		}
	}
}

// Under batch isolation an own-login child never sees another profile's credential var, but keeps its native var.
func TestChildCmdOwnLoginDropsForeignCredentialsUnderIsolation(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{`+
		`"pa":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"PA_BATCH_KEY"},"models":{"opus":"m"}},`+
		`"pb":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"OPENCODE_API_KEY"},"models":{"opus":"m"}}}}}`)
	t.Setenv("PA_BATCH_KEY", "foreign-secret")
	has := func(cmd interface{ Environ() []string }, kv string) bool { return slices.Contains(cmd.Environ(), kv) }
	iso, _ := ChildCmd(withIsolation(context.Background()), "/bin/true", nil, []string{"OPENCODE_API_KEY=native"}, "", "", nil, true)
	if has(iso, "PA_BATCH_KEY=foreign-secret") || !has(iso, "OPENCODE_API_KEY=native") {
		t.Errorf("isolated env leaks PA_BATCH_KEY or lost the native var: %d", len(iso.Env))
	}
	plain, _ := ChildCmd(context.Background(), "/bin/true", nil, nil, "", "", nil, false) // claude: no agent scrub
	if !has(plain, "PA_BATCH_KEY=foreign-secret") {
		t.Error("outside a batch the env must stay unchanged")
	}
}

// A vendor probe holds no registry credential and no own login but the one it names; a plain ctx scrubs none of them.
func TestProbeContextScrubsCredentials(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks",`+
		`"host":"https://dev.cloud.databricks.com","auth":{"type":"key","env":"DEV_WS_TOKEN"}}}}}`)
	planted := []string{"DEV_WS_TOKEN", "OPENCODE_API_KEY", "GITHUB_TOKEN", "GH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}
	for _, k := range planted {
		t.Setenv(k, "SENTINEL")
	}
	creds := func(ctx context.Context, mode EnvMode) []string {
		names := envNames(EnvSpec{Mode: mode}.Environ(ctx))
		return slices.DeleteFunc(names, func(n string) bool { return !slices.Contains(planted, n) })
	}
	wants := map[string][]string{"": nil, "opencode": {"OPENCODE_API_KEY"}, "copilot": {"GH_TOKEN", "GITHUB_TOKEN"}}
	for login, want := range wants {
		if got := creds(ProbeContext(context.Background(), login), EnvAgent); !slices.Equal(slices.Sorted(slices.Values(got)), want) {
			t.Errorf("probe for login %q keeps %v, want %v", login, got, want)
		}
	}
	if got := creds(context.Background(), EnvClaude); len(got) != 5 {
		t.Errorf("a claude child keeps %v, want all five planted names", got)
	}
}

// A hub dispatch keeps the credential its launch supplies even when another profile declares the same env name.
func TestChildCmdLaunchValuesSurviveIsolationScrub(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{`+
		`"ocgo":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"OCGO_KEY"},"models":{"opus":"m"}},`+
		`"other":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"OPENCODE_API_KEY"},"models":{"opus":"m"}}}}}`)
	t.Setenv("OPENCODE_API_KEY", "inherited-foreign")
	reg, err := providers.Load()
	if err != nil || reg.Providers["ocgo"] == nil {
		t.Fatalf("registry: %v", err)
	}
	ctx := withOwnProvider(withIsolation(context.Background()), reg.Providers["ocgo"])
	cmd, _ := ChildCmd(ctx, "/bin/true", nil, []string{"OPENCODE_API_KEY=launch"}, "", "", nil, true)
	if !slices.Contains(cmd.Env, "OPENCODE_API_KEY=launch") || slices.Contains(cmd.Env, "OPENCODE_API_KEY=inherited-foreign") {
		t.Errorf("env keeps the inherited foreign value or lost the launch's: %d", len(cmd.Env))
	}
}

// Given an inherited PATH holding the rtk shim dir, Then the child env drops it and keeps the other entries.
func TestEnvironDropsShimPath(t *testing.T) {
	shim := rtkwrap.Dir()
	base := []string{"PATH=" + strings.Join([]string{shim, "/usr/bin", shim + "/", "/bin"}, string(os.PathListSeparator))}
	for _, e := range (EnvSpec{Mode: EnvAgent, Base: base}).Environ(context.Background()) {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			if want := "/usr/bin" + string(os.PathListSeparator) + "/bin"; !strings.HasSuffix(v, want) || strings.Contains(v, shim) {
				t.Errorf("PATH = %q, want suffix %q and no shim", v, want)
			}
			return
		}
	}
	t.Error("PATH missing from the child env")
}

func TestEnvironExportsChosenProxy(t *testing.T) {
	setConfigDir(t)
	t.Setenv(dispatchProxyEnv, "stale:old")
	ctx := withDispatchID(t.Context(), "pxid", 1)
	routeMu.Lock()
	chosenByID["pxid"] = "phase:g1"
	routeMu.Unlock()
	t.Cleanup(func() { forgetChosenProxy(ctx) })
	if env := (EnvSpec{Mode: EnvClaude}).Environ(ctx); !slices.Contains(env, dispatchProxyEnv+"=phase:g1") {
		t.Fatalf("chosen proxy not exported: %v", slices.DeleteFunc(env, func(e string) bool { return !strings.Contains(e, "PROXY") }))
	}
}

func TestEnvironDispatchProxyKeepsInheritedAndTypedEnv(t *testing.T) {
	setConfigDir(t)
	t.Setenv(dispatchProxyEnv, "phase:g1")
	ctx := withDispatchID(t.Context(), "pxid2", 1)
	if env := (EnvSpec{Mode: EnvClaude}).Environ(ctx); !slices.Contains(env, dispatchProxyEnv+"=phase:g1") {
		t.Fatal("no choice must keep the inherited value")
	}
	routeMu.Lock()
	chosenByID["pxid2"] = "shared"
	routeMu.Unlock()
	t.Cleanup(func() { forgetChosenProxy(ctx) })
	if env := (EnvSpec{Mode: EnvClaude}).Environ(ctx); slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, dispatchProxyEnv+"=") }) {
		t.Fatal("a daemon choice must clear the inherited value")
	}
	typed := dispatchProxyEnv + "=mine:x"
	if env := (EnvSpec{Mode: EnvClaude, Extra: []string{typed}}).Environ(ctx); !slices.Contains(env, typed) {
		t.Fatal("a typed --env value must survive")
	}
}

func TestParentTeamFirst(t *testing.T) {
	cfg, _ := proxyteam.Load([]byte(`{"schema":1,"teams":[{"name":"phase","kinds":["group"],"host":"phase","lifetime":"phase",` +
		`"reserved":1,"borrow":"never","fallback":["shared"]}]}`))
	t.Setenv(dispatchProxyEnv, "phase:g1")
	got := parentTeamFirst(cfg, cfg.Routes(proxyteam.KindNested))
	if len(got) == 0 || got[0].Name != "phase" || got[len(got)-1].Name != "shared" {
		t.Fatalf("routes = %v", got)
	}
}

// UnitOf keeps an endpoint, as and account on the same provider only.
func TestUnitOfKeepsEndpointOnSameProvider(t *testing.T) {
	own := agentconfig.Overlay{Endpoint: "e", Provider: "p", Profile: "x", Identity: "sp", Account: "a"}
	if got := UnitOf(own, "p", "y"); got.Endpoint != "e" || got.Identity != "sp" || got.Account != "a" || got.Profile != "y" {
		t.Errorf("same provider: %+v", got)
	}
	if got := UnitOf(own, "q", "y"); got.Endpoint != "" || got.Identity != "" || got.Account != "" {
		t.Errorf("other provider: %+v", got)
	}
}

// A provider-only unit replaces the base's route whole, its endpoint included.
func TestDeriveUnitProviderOnlyDropsEndpoint(t *testing.T) {
	base := agentconfig.Overlay{Endpoint: "https://e.example/v1", Provider: "openai", Profile: "x"}
	got, err := DeriveUnit(base, agentconfig.Overlay{Provider: "google"}, agentconfig.HopAppend)
	if err != nil || got.Endpoint != "" || got.Provider != "google" || got.Profile != "" {
		t.Errorf("got %+v, %v; want provider google alone", got, err)
	}
}
