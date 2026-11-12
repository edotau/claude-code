package tailscale

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// stubCLI points lookPathReal/execCommandContext at a fake `tailscale` script that branches on "$@".
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
	t.Cleanup(StubMacAppCLI("")) // the GUI app's bundled binary would otherwise answer on a Mac that has it
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
	if _, err := Status(context.Background()); !errors.Is(err, errNoCLI) {
		t.Errorf("Status() err = %v, want errNoCLI", err)
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
	if !errors.Is(err, errNotRunning) {
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
	if err := connect(context.Background(), "tskey-auth-xxx", true); err != nil {
		t.Fatalf("Connect() err = %v, want nil (no-op)", err)
	}
	if _, err := os.Stat(calls); err == nil {
		t.Error("Connect ran `tailscale up` although status was already Running and approved")
	}
}

// TestConnectRunsUpWithAuthKeyAsFileNeverArgv: the key reaches `up` via --auth-key=file:, never as argv.
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

	if err := connect(context.Background(), "tskey-auth-secretvalue", true); err != nil {
		t.Fatalf("Connect() err = %v", err)
	}
	keyFile := authKeyFileFromArgv(t, argvFile)
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

// authKeyFileFromArgv reads the recorded `up` argv, asserts the key never rode on it and returns the key file path.
func authKeyFileFromArgv(t *testing.T, argvFile string) string {
	t.Helper()
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("`tailscale up` was not invoked: %v", err)
	}
	if strings.Contains(string(argv), "tskey-") {
		t.Fatalf("auth key must never appear in argv, got %q", string(argv))
	}
	fields := strings.Fields(string(argv))
	if len(fields) != 3 || fields[0] != "up" || !strings.HasPrefix(fields[1], "--auth-key=file:") ||
		fields[2] != "--advertise-exit-node" {
		t.Fatalf("up argv = %q, want up + --auth-key=file:<path> + --advertise-exit-node", argv)
	}
	return strings.TrimPrefix(fields[1], "--auth-key=file:")
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
		if err := advertiseExitNode(context.Background(), tc.on); err != nil {
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
	t.Setenv("PATH", dir) // also starves the keyring tool so vault.Get can't find a stray credential
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

func TestUseExitNodeFlags(t *testing.T) {
	for _, tc := range []struct {
		peer string
		want string
	}{
		{"peer-node", "set\n--exit-node=peer-node\n--exit-node-allow-lan-access=true\n"},
		{"", "set\n--exit-node=\n"},
	} {
		dir := t.TempDir()
		argvFile := filepath.Join(dir, "argv")
		bin := fakeBin(t, dir, "tailscale", "printf '%s\\n' \"$@\" > "+shQuote(argvFile))
		prevLook, prevExec := lookPathReal, execCommandContext
		lookPathReal = func(string) (string, error) { return bin, nil }
		execCommandContext = exec.CommandContext
		if err := useExitNode(context.Background(), tc.peer); err != nil {
			t.Fatalf("UseExitNode(%q) err = %v", tc.peer, err)
		}
		b, _ := os.ReadFile(argvFile)
		if string(b) != tc.want {
			t.Errorf("UseExitNode(%q) argv = %q, want %q", tc.peer, string(b), tc.want)
		}
		lookPathReal, execCommandContext = prevLook, prevExec
	}
}

// shQuote single-quotes s for a shell script literal.
func shQuote(s string) string {
	return "'" + s + "'"
}

func TestAdvertiseRoutesFlags(t *testing.T) {
	for _, tc := range []struct {
		routes []string
		want   string
	}{
		{[]string{"192.168.1.0/24", "10.9.0.0/16"}, "set\n--advertise-routes=192.168.1.0/24,10.9.0.0/16\n"},
		{nil, "set\n--advertise-routes=\n"},
	} {
		dir := t.TempDir()
		argvFile := filepath.Join(dir, "argv")
		bin := fakeBin(t, dir, "tailscale", "printf '%s\\n' \"$@\" > "+shQuote(argvFile))
		prevLook, prevExec := lookPathReal, execCommandContext
		lookPathReal = func(string) (string, error) { return bin, nil }
		execCommandContext = exec.CommandContext
		if err := advertiseRoutes(context.Background(), tc.routes); err != nil {
			t.Fatalf("AdvertiseRoutes(%v) err = %v", tc.routes, err)
		}
		b, _ := os.ReadFile(argvFile)
		if string(b) != tc.want {
			t.Errorf("AdvertiseRoutes(%v) argv = %q, want %q", tc.routes, string(b), tc.want)
		}
		lookPathReal, execCommandContext = prevLook, prevExec
	}
}

// TestStatusSubnetRoutes: approved LAN routes show up; own /32,/128 and the default routes do not.
func TestStatusSubnetRoutes(t *testing.T) {
	stubCLI(t, catScript(t, "status_subnet.json"))
	st, err := Status(context.Background())
	if err != nil {
		t.Fatalf("Status err = %v", err)
	}
	if len(st.SubnetRoutes) != 1 || st.SubnetRoutes[0] != "192.168.1.0/24" {
		t.Errorf("SubnetRoutes = %v, want [192.168.1.0/24]", st.SubnetRoutes)
	}
	if !st.ExitNodeApproved {
		t.Error("ExitNodeApproved = false with both default routes present")
	}
}

// `tailscale up` registers a node and needs longer than the quick-call defaultExecTimeout.
func TestConnectUpOutlivesDefaultExecTimeout(t *testing.T) {
	dir := t.TempDir()
	bin := fakeBin(t, dir, "tailscale", catScript(t, "status_stopped.json"))
	prevLook, prevExec := lookPathReal, execCommandContext
	lookPathReal = func(string) (string, error) { return bin, nil }
	var upBudget time.Duration
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "up" {
			if dl, ok := ctx.Deadline(); ok {
				upBudget = time.Until(dl)
			}
		}
		return exec.CommandContext(ctx, name, args...)
	}
	t.Cleanup(func() { lookPathReal, execCommandContext = prevLook, prevExec })

	if err := connect(context.Background(), "tskey-auth-x", false); err != nil {
		t.Fatalf("Connect() err = %v", err)
	}
	if upBudget <= defaultExecTimeout {
		t.Errorf("`up` deadline budget = %v, want > %v", upBudget, defaultExecTimeout)
	}
}

// peersByAddr indexes every peer address, and this node's own addresses carry its Running state.
func TestPeersByAddrIndexesPeersAndSelf(t *testing.T) {
	st := Local{
		BackendState: "Running", IPs: []string{"100.0.0.1"}, ExitNodeOption: true,
		Peers: []peer{{IPs: []string{"100.0.0.2", "fd7a::2"}, Online: true, ExitNode: true}},
	}
	got := peersByAddr(st)
	if len(got) != 3 {
		t.Fatalf("peersByAddr = %+v, want 3 addresses", got)
	}
	if p := got["fd7a::2"]; !p.Online || !p.ExitNode {
		t.Errorf("peer address = %+v, want the online exit-node peer", p)
	}
	if self := got["100.0.0.1"]; !self.Online || self.ExitNode || !self.ExitNodeOption {
		t.Errorf("self address = %+v, want Online (Running), ExitNodeOption and not ExitNode", self)
	}
	st.BackendState = "Stopped"
	if got := peersByAddr(st)["100.0.0.1"]; got.Online {
		t.Errorf("stopped self = %+v, want offline", got)
	}
}

// A CLI that prints past the cap is cut off with an error, not buffered whole.
func TestStatusCapsOutput(t *testing.T) {
	stubCLI(t, `/usr/bin/head -c 3145728 /dev/zero`)
	if _, err := Status(context.Background()); err == nil || !strings.Contains(err.Error(), "over") {
		t.Errorf("Status over cap: err = %v, want an over-cap error", err)
	}
}

// On darwin without a PATH tailscale, the GUI app's bundled CLI is used when executable; elsewhere errNoCLI.
func TestTailscaleBinMacAppFallback(t *testing.T) {
	noCLI(t)
	dir := t.TempDir()
	app := fakeBin(t, dir, "Tailscale", "exit 0")
	noExec := filepath.Join(dir, "plain")
	if err := os.WriteFile(noExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	prevOS, prevApp := goos, macAppCLI
	t.Cleanup(func() { goos, macAppCLI = prevOS, prevApp })
	for _, c := range []struct{ os, path, want string }{
		{"darwin", app, app},
		{"linux", app, ""},
		{"darwin", noExec, ""},
		{"darwin", filepath.Join(dir, "missing"), ""},
	} {
		goos, macAppCLI = c.os, c.path
		got, err := tailscaleBin()
		if got != c.want || (c.want == "") != (err != nil) {
			t.Errorf("goos=%s path=%s: got (%q, %v), want %q", c.os, filepath.Base(c.path), got, err, c.want)
		}
	}
}

// TestMachinesFetchesDevicesAndStatusConcurrently: each call waits to see the other started; serial order fails one.
func TestMachinesFetchesDevicesAndStatusConcurrently(t *testing.T) {
	hermeticSecrets(t)
	t.Setenv("TAILSCALE_API_KEY", "tskey-api-abc")
	dir := t.TempDir()
	statusMark, devicesMark := filepath.Join(dir, "status"), filepath.Join(dir, "devices")
	// status starts, then waits up to 2s for the devices call; without it, it fails and Online stays false.
	stubCLI(t, ": > "+shQuote(statusMark)+"\ni=0\nwhile [ ! -e "+shQuote(devicesMark)+" ]; do\n"+
		"  i=$((i+1)); [ $i -gt 200 ] && exit 1; /bin/sleep 0.01\ndone\n"+catScript(t, "status_running.json"))
	var sawStatus atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = os.WriteFile(devicesMark, nil, 0o600)
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(statusMark); err == nil {
				sawStatus.Store(true)
				break
			}
		}
		_, _ = w.Write([]byte(`{"devices":[{"name":"test-phone.tailxxxxx.ts.net","addresses":["100.105.201.90"]}]}`))
	}))
	t.Cleanup(srv.Close)
	prev := apiBaseURL
	apiBaseURL = srv.URL
	t.Cleanup(func() { apiBaseURL = prev })
	machines, err := Machines(context.Background())
	if err != nil || len(machines) != 1 {
		t.Fatalf("Machines() = %+v, %v; want one api device", machines, err)
	}
	if !sawStatus.Load() || !machines[0].Online {
		t.Errorf("serial calls: devices saw status=%v, status overlay online=%v; want both true",
			sawStatus.Load(), machines[0].Online)
	}
	if got := CachedMachineNames(); !slices.Equal(got, []string{"test-phone.tailxxxxx.ts.net"}) {
		t.Errorf("machine cache = %q, want the live listing", got)
	}
}
