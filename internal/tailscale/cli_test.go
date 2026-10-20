package tailscale

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBin writes an executable shell script named name into dir and returns its path.
func fakeBin(t *testing.T, dir, name, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fakeBin requires a POSIX shell")
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// stubCLI points lookPathReal/execCommandContext at a fake `tailscale` script for the test's
// duration; the script receives "$@" so it can branch on the subcommand.
func stubCLI(t *testing.T, script string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	bin := fakeBin(t, dir, "tailscale", script)
	prevLook, prevExec := lookPathReal, execCommandContext
	lookPathReal = func(string) (string, error) { return bin, nil }
	execCommandContext = exec.CommandContext
	t.Cleanup(func() { lookPathReal, execCommandContext = prevLook, prevExec })
	return dir
}

// noCLI makes tailscaleBin fail as if the binary were absent from PATH.
func noCLI(t *testing.T) {
	t.Helper()
	prev := lookPathReal
	lookPathReal = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { lookPathReal = prev })
}

func catScript(t *testing.T, fixture string) string {
	t.Helper()
	path := filepath.Join("testdata", fixture)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return "if [ \"$1\" = status ]; then /bin/cat <<'EOF'\n" + string(b) + "\nEOF\nfi"
}

func TestStatusNoCLI(t *testing.T) {
	noCLI(t)
	if _, err := Status(context.Background()); !errors.Is(err, ErrNoCLI) {
		t.Errorf("Status() err = %v, want ErrNoCLI", err)
	}
}

func TestStatusParsesRunningFixture(t *testing.T) {
	stubCLI(t, catScript(t, "status_running.json"))
	local, err := Status(context.Background())
	if err != nil {
		t.Fatalf("Status() err = %v, want nil (Running)", err)
	}
	if local.BackendState != "Running" {
		t.Errorf("BackendState = %q, want Running", local.BackendState)
	}
	if local.DNSName != "test-mac.tailxxxxx.ts.net." {
		t.Errorf("DNSName = %q", local.DNSName)
	}
	if !local.ExitNodeOption {
		t.Error("ExitNodeOption = false, want true (sampled fixture advertises the option)")
	}
	if !local.ExitNodeApproved {
		t.Error("ExitNodeApproved = false, want true (Self.AllowedIPs carries both default routes)")
	}
	if len(local.Peers) != 1 || local.Peers[0].DNSName != "test-phone.tailxxxxx.ts.net." {
		t.Errorf("Peers = %+v, want one peer test-phone…", local.Peers)
	}
}

func TestStatusNotApprovedFixture(t *testing.T) {
	stubCLI(t, catScript(t, "status_not_approved.json"))
	local, err := Status(context.Background())
	if err != nil {
		t.Fatalf("Status() err = %v", err)
	}
	if local.ExitNodeApproved {
		t.Error("ExitNodeApproved = true, want false (no default routes in AllowedIPs)")
	}
}

func TestStatusNotRunningFixture(t *testing.T) {
	stubCLI(t, catScript(t, "status_stopped.json"))
	local, err := Status(context.Background())
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Status() err = %v, want ErrNotRunning", err)
	}
	if local.BackendState != "Stopped" {
		t.Errorf("Local is still returned on ErrNotRunning; BackendState = %q, want Stopped", local.BackendState)
	}
}

func TestConnectNoOpWhenAlreadyRunning(t *testing.T) {
	dir := stubCLI(t, catScript(t, "status_running.json")+"\nif [ \"$1\" = up ]; then echo UP_CALLED >> \"$TS_CALLS\"; fi")
	calls := filepath.Join(dir, "calls")
	t.Setenv("TS_CALLS", calls)
	// exitNode=true: fixture is already approved, so this must stay a no-op too.
	if err := Connect(context.Background(), "tskey-auth-xxx", true); err != nil {
		t.Fatalf("Connect() err = %v, want nil (no-op)", err)
	}
	if _, err := os.Stat(calls); err == nil {
		t.Error("Connect ran `tailscale up` although status was already Running and approved")
	}
}

// TestConnectRunsUpWithAuthKeyAsFileNeverArgv: the auth key must never appear as a `ps`-visible argv
// element — Connect writes it to a temp file and passes --auth-key=file:<path>, removed once `up` exits.
// The fake `tailscale up` records its own argv and separately captures what the file: path contained,
// proving the key reached the CLI without ever riding on the command line.
func TestConnectRunsUpWithAuthKeyAsFileNeverArgv(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	keyContentFile := filepath.Join(dir, "key-content")
	script := catScript(t, "status_stopped.json") +
		"\nif [ \"$1\" = up ]; then\n" +
		"  printf '%s\\n' \"$@\" > " + shQuote(argvFile) + "\n" +
		"  for a in \"$@\"; do\n" +
		"    case \"$a\" in\n" +
		"      --auth-key=file:*) cat \"${a#--auth-key=file:}\" > " + shQuote(keyContentFile) + " ;;\n" +
		"    esac\n" +
		"  done\n" +
		"fi"
	bin := fakeBin(t, dir, "tailscale", script)
	prevLook, prevExec := lookPathReal, execCommandContext
	lookPathReal = func(string) (string, error) { return bin, nil }
	execCommandContext = exec.CommandContext
	t.Cleanup(func() { lookPathReal, execCommandContext = prevLook, prevExec })

	if err := Connect(context.Background(), "tskey-auth-secretvalue", true); err != nil {
		t.Fatalf("Connect() err = %v", err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("`tailscale up` was not invoked: %v", err)
	}
	if strings.Contains(string(argv), "tskey-") {
		t.Fatalf("auth key must never appear in argv, got %q", string(argv))
	}
	fields := strings.Fields(string(argv))
	if len(fields) != 3 || fields[0] != "up" || !strings.HasPrefix(fields[1], "--auth-key=file:") || fields[2] != "--advertise-exit-node" {
		t.Fatalf("up argv = %q, want up + --auth-key=file:<path> + --advertise-exit-node", argv)
	}
	keyFile := strings.TrimPrefix(fields[1], "--auth-key=file:")
	content, err := os.ReadFile(keyContentFile)
	if err != nil {
		t.Fatalf("tailscale up never read the --auth-key=file: path: %v", err)
	}
	if string(content) != "tskey-auth-secretvalue" {
		t.Fatalf("key file content = %q, want the auth key", content)
	}
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Fatalf("key temp file %q should be removed once Connect returns, stat err = %v", keyFile, err)
	}
}

func TestAdvertiseExitNodeFlags(t *testing.T) {
	for _, tc := range []struct {
		on   bool
		want string
	}{
		{true, "set\n--advertise-exit-node\n"},
		{false, "set\n--advertise-exit-node=false\n"},
	} {
		dir := t.TempDir()
		argvFile := filepath.Join(dir, "argv")
		bin := fakeBin(t, dir, "tailscale", "printf '%s\\n' \"$@\" > "+shQuote(argvFile))
		prevLook, prevExec := lookPathReal, execCommandContext
		lookPathReal = func(string) (string, error) { return bin, nil }
		execCommandContext = exec.CommandContext
		if err := AdvertiseExitNode(context.Background(), tc.on); err != nil {
			t.Fatalf("AdvertiseExitNode(%v) err = %v", tc.on, err)
		}
		b, _ := os.ReadFile(argvFile)
		if string(b) != tc.want {
			t.Errorf("AdvertiseExitNode(%v) argv = %q, want %q", tc.on, string(b), tc.want)
		}
		lookPathReal, execCommandContext = prevLook, prevExec
	}
}

func TestMachinesFallsBackToStatusWithoutCredential(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_SECRET", "")
	t.Setenv("TAILSCALE_API_KEY", "")
	dir := stubCLI(t, catScript(t, "status_running.json"))
	t.Setenv("PATH", dir) // also starves the keyring tool so secrets.Get can't find a stray credential
	machines, err := Machines(context.Background())
	if err != nil {
		t.Fatalf("Machines() err = %v", err)
	}
	if len(machines) != 2 {
		t.Fatalf("Machines() = %d entries, want 2 (self + one peer)", len(machines))
	}
	if machines[0].Source != "cli" || machines[1].Source != "cli" {
		t.Errorf("Machines() sources = %q/%q, want cli/cli", machines[0].Source, machines[1].Source)
	}
}

// shQuote single-quotes s for a shell script literal.
func shQuote(s string) string {
	return "'" + s + "'"
}
