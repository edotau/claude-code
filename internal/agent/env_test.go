package agent

import (
	"bytes"
	"context"
	"errors"
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

	"github.com/edotau/claude-code/internal/paths/pathstest"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"

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
			t.Errorf("%+v: %s present %d times, want 1", s, want, n)
		}
	}
}

// Given an outer dispatch's policy in the env, Then a nested policy keeps its Deny rules and plan mode, never fewer.
func TestInheritPermissionsKeepsOuterDeny(t *testing.T) {
	t.Setenv(DispatchPermissionsEnv, `{"mode":"plan","deny":[{"Tool":"Bash","Arg":"rm *"}]}`)
	inner, err := ParsePermissions("auto", []string{"Bash"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := inheritPermissions(inner)
	if err != nil || got.Mode != "plan" || got.Decide("Bash", "rm x") != Deny || got.Decide("Bash", "ls") != Allow {
		t.Fatalf("inherit = %+v, %v; want plan mode, rm denied, ls allowed", got, err)
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
	cmd, tail := ChildCmd(ctx, "/bin/sh", []string{"-c", "sleep 30 & sleep 30"}, nil, "", "", nil, false)
	start := time.Now()
	err := ChildErr(ctx, "sh", cmd.Run(), tail)
	if time.Since(start) > 3*time.Second || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("grandchild outlived the deadline: took %v, err %v", time.Since(start), err)
	}
}

// ChildCmd sets $PWD with cmd.Dir for a child reading $PWD; an empty dir leaves PWD unset so the child inherits cwd.
func TestChildCmdSetsPWD(t *testing.T) {
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

// A vendor CLI child never sees HARNESS_VAULT_KEY (it would open the vault outside the harness's own
// process); the claude agent's own child keeps it since hooks need it to read the vault.
func TestChildCmdScrubsVaultKeyForVendorOnly(t *testing.T) {
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
			t.Errorf("own=%v: scrubbed env = %q, want %q", own, got, want)
		}
	}
}

// Under batch isolation an own-login child never sees another profile's custom credential var, but keeps the
// native var its launch sets; outside a batch nothing is scrubbed.
func TestChildCmdOwnLoginDropsForeignCredentialsUnderIsolation(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{`+
		`"pa":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"PA_BATCH_KEY"},"models":{"opus":"m"}},`+
		`"pb":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"OPENCODE_API_KEY"},"models":{"opus":"m"}}}}}`)
	t.Setenv("PA_BATCH_KEY", "foreign-secret")
	has := func(cmd interface{ Environ() []string }, kv string) bool { return slices.Contains(cmd.Environ(), kv) }
	iso, _ := ChildCmd(withIsolation(context.Background()), "/bin/true", nil, []string{"OPENCODE_API_KEY=native"}, "", "", nil, true)
	if has(iso, "PA_BATCH_KEY=foreign-secret") || !has(iso, "OPENCODE_API_KEY=native") {
		t.Errorf("isolated env leaks PA_BATCH_KEY or lost the native var: %v", iso.Env)
	}
	plain, _ := ChildCmd(context.Background(), "/bin/true", nil, nil, "", "", nil, true)
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
	creds := func(ctx context.Context) []string {
		names := envNames(EnvSpec{Mode: EnvAgent}.Environ(ctx))
		return slices.DeleteFunc(names, func(n string) bool { return !slices.Contains(planted, n) })
	}
	wants := map[string][]string{"": nil, "opencode": {"OPENCODE_API_KEY"}, "copilot": {"GH_TOKEN", "GITHUB_TOKEN"}}
	for login, want := range wants {
		if got := creds(ProbeContext(context.Background(), login)); !slices.Equal(slices.Sorted(slices.Values(got)), want) {
			t.Errorf("probe for login %q keeps %v, want %v", login, got, want)
		}
	}
	if got := creds(context.Background()); len(got) != 5 {
		t.Errorf("a plain child keeps %v, want all five planted names", got)
	}
}

// A hub dispatch keeps the credential its launch supplies even when another profile declares the same env name; the
// inherited value of that name is gone.
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
		t.Errorf("env keeps the inherited foreign value or lost the launch's: %v", slices.DeleteFunc(slices.Clone(cmd.Env),
			func(kv string) bool { return !strings.HasPrefix(kv, "OPENCODE_API_KEY") }))
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
