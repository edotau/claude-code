package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/paths/pathstest"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"

	"github.com/edotau/claude-code/internal/egress"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/proc"
)

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

// Given an outer dispatch's policy in the env, Then a nested policy keeps its Deny rules and plan mode, never fewer.
func TestInheritPermissionsKeepsOuterDeny(t *testing.T) {
	t.Setenv(DispatchPermissionsEnv, `{"mode":"plan","deny":[{"Tool":"Bash","Arg":"rm *"}],"ask":[{"Tool":"Bash","Arg":"curl *"}]}`)
	inner, err := ParsePermissions("auto", []string{"Bash"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := inheritPermissions(inner)
	if err != nil || got.Mode != "plan" || got.Decide("Bash", "rm x") != Deny || got.Decide("Bash", "ls") != Allow {
		t.Fatalf("inherit = %+v, %v; want plan mode, rm denied, ls allowed", got, err)
	}
	if got.Decide("Bash", "curl x") != Ask { // the inner Bash allow must not reopen the outer ask
		t.Fatalf("inherit = %+v; want the outer curl ask kept", got)
	}
	if got, _ := inheritPermissions(Permissions{}); len(got.Deny) != 1 {
		t.Fatalf("no inner policy: got %+v, want the outer policy whole", got)
	}
	t.Setenv(DispatchPermissionsEnv, "{bad")
	if _, err := inheritPermissions(inner); err == nil {
		t.Fatal("malformed outer policy: want an error (fail closed)")
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
	t.Setenv(memory.OriginEnv, "/inherited") // already inherited, so withMemoryOrigin adds nothing either
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

// Given a real child under ChildCmd with a grandchild, When ctx is cancelled, Then no process stays in the group.
func TestChildCmdCancelLeavesNoProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are unix-only")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, _ := ChildCmd(ctx, "sh", []string{"-c", "sleep 60 & sleep 60; wait"}, nil, t.TempDir(), "", nil, false)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	time.Sleep(100 * time.Millisecond)
	if err := exec.Command("kill", "-0", "--", "-"+strconv.Itoa(pgid)).Run(); err != nil {
		t.Fatalf("group %d not alive before cancel: %v", pgid, err)
	}
	cancel()
	var ee *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &ee) {
		t.Fatalf("wait = %v, want the kill's exit error", err)
	}
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if out, err := exec.Command("kill", "-0", "--", "-"+strconv.Itoa(pgid)).CombinedOutput(); err != nil &&
			strings.Contains(string(out), "No such process") {
			return
		}
	}
	t.Errorf("process group %d still has members after cancel", pgid)
}

// An ambient but empty COWORKER_OVERLAY must not hide the vault's stored overlay from the captured snapshot.
func TestRunValuesAmbientEmptyOverlayKeepsStored(t *testing.T) {
	vaulttest.Use(t)
	const stored = `{"provider":"databricks","profile":"qa"}`
	vaulttest.Put(t, vault.Env, agentconfig.OverlayVar, stored)
	t.Setenv(agentconfig.OverlayVar, "")
	ctx, _ := RunValues(context.Background(), nil)
	if got := agentconfig.DispatchOverlayContext(ctx, ""); got != stored {
		t.Fatalf("DispatchOverlayContext = %q, want the stored overlay %q", got, stored)
	}
}

// Concurrent child launches keep independent snapshots and request-pinned egress without changing the parent.
func TestRunValuesConcurrentChildren(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh")
	t.Setenv("PST_RUN", "parent")
	t.Setenv(egress.EnvHost, "parent-host")
	values := proc.ValuesFromEnv(os.Environ())
	start := make(chan struct{})
	errc := make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		go func() {
			<-start
			ctx, copied := RunValues(context.Background(), values.With("PST_RUN", name))
			copied["PST_RUN"] = "mutated-after-capture"
			ctx = egress.WithHost(ctx, name+"-host")
			for _, agent := range []string{"claude", "codex", "copilot", "gemini", "opencode"} {
				cmd, _ := ChildCmd(ctx, "/bin/sh", []string{"-c", `printf '%s|%s' "$PST_RUN" "$HARNESS_EGRESS_HOST"`},
					nil, "", "", nil, agent != "claude")
				out, err := cmd.Output()
				if err != nil {
					errc <- err
					return
				}
				if string(out) != name+"|"+name+"-host" {
					errc <- fmt.Errorf("%s child got %q", agent, out)
					return
				}
			}
			direct := proc.ValuesFromEnv(EnvSpec{Mode: EnvClaude}.Environ(egress.WithHost(ctx, "")))
			if direct[egress.EnvHost] != "" {
				errc <- errors.New("explicit direct host lost")
				return
			}
			errc <- nil
		}()
	}
	close(start)
	for range 2 {
		if err := <-errc; err != nil {
			t.Error(err)
		}
	}
	if os.Getenv("PST_RUN") != "parent" || os.Getenv(egress.EnvHost) != "parent-host" {
		t.Fatal("child snapshot changed the parent")
	}
}
