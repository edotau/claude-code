package vscode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/proc"

	"github.com/edotau/claude-code/internal/paths"
)

// setup isolates StateDir() in a fresh temp dir per test, matching the router tests' pattern.
func setup(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	return paths.StateDir()
}

// --- daemon state file paths -----------------------------------------------------------------

func TestClientSecretCreatesReusesAndIsPrivate(t *testing.T) {
	setup(t)
	secretPath := filepath.Join(paths.StateDir(), "vscode-serve.secret")

	s1, err := ClientSecret()
	if err != nil {
		t.Fatalf("ClientSecret: %v", err)
	}
	if s1 == "" {
		t.Fatal("ClientSecret: empty secret")
	}
	fi, err := os.Stat(secretPath)
	if err != nil {
		t.Fatalf("stat %s: %v", secretPath, err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("vscode-serve.secret mode = %o, want 0600", fi.Mode().Perm())
	}

	s2, err := ClientSecret()
	if err != nil {
		t.Fatalf("ClientSecret (second call): %v", err)
	}
	if s1 != s2 {
		t.Fatalf("ClientSecret not reused: %q != %q", s1, s2)
	}
}

func TestStateFileAndLogFilePaths(t *testing.T) {
	setup(t)
	if got, want := stateFile(), filepath.Join(paths.StateDir(), "vscode-serve.json"); got != want {
		t.Fatalf("stateFile() = %q, want %q", got, want)
	}
	if got, want := LogFile(), filepath.Join(paths.StateDir(), "vscode-serve.log"); got != want {
		t.Fatalf("LogFile() = %q, want %q", got, want)
	}
}

// --- serve-web argv ----------------------------------------------------------------------------

func TestServeWebArgv(t *testing.T) {
	got := ServeWebArgv("code", "/tmp/secret", 7780, "/tmp/data", "")
	want := []string{
		"code", "serve-web",
		"--host", "127.0.0.1",
		"--port", "7780",
		"--accept-server-license-terms",
		"--connection-token-file", "/tmp/secret",
		"--server-data-dir", "/tmp/data",
	}
	if len(got) != len(want) {
		t.Fatalf("ServeWebArgv() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ServeWebArgv()[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// --- public URL ---------------------------------------------------------------------------------

func TestPublicURLWithDNSName(t *testing.T) {
	got := PublicURL("my-machine.tailnetxyz.ts.net.", "203.0.113.1", 7780, "abc123")
	want := "https://my-machine.tailnetxyz.ts.net/?tkn=abc123"
	if got != want {
		t.Fatalf("PublicURL() = %q, want %q", got, want)
	}
}

func TestPublicURLFallsBackToIPWithoutDNSName(t *testing.T) {
	got := PublicURL("", "203.0.113.1", 7780, "abc123")
	want := "http://203.0.113.1:7780/?tkn=abc123"
	if got != want {
		t.Fatalf("PublicURL() fallback = %q, want %q", got, want)
	}
}

// --- tailscale argv -------------------------------------------------------------------------

func TestTailscaleServeArgv(t *testing.T) {
	got := TailscaleServeArgv(7780, false)
	want := []string{"serve", "--bg", "--https=443", "http://127.0.0.1:7780"}
	if len(got) != len(want) {
		t.Fatalf("TailscaleServeArgv(funnel=false) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("TailscaleServeArgv(funnel=false)[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestTailscaleServeArgvFunnelDiffersFromBase(t *testing.T) {
	base := TailscaleServeArgv(7780, false)
	funnel := TailscaleServeArgv(7780, true)
	if equalStrings(base, funnel) {
		t.Fatalf("funnel variant must differ from the base serve argv, got identical: %v", base)
	}
	// Exact flag the implementation uses for the funnel variant is not pinned by the contract;
	// this just guards that --https=443 (the fixed port) survives into that variant too.
	if !containsString(funnel, "--https=443") {
		t.Fatalf("TailscaleServeArgv(funnel=true) = %v, want it to retain --https=443", funnel)
	}
}

func TestTailscaleOffArgv(t *testing.T) {
	got := TailscaleOffArgv()
	if !containsString(got, "off") {
		t.Fatalf("TailscaleOffArgv() = %v, want an \"off\" argument somewhere", got)
	}
	if !containsString(got, "--https=443") {
		t.Fatalf("TailscaleOffArgv() = %v, want it to target --https=443", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(s []string, want string) bool {
	return slices.Contains(s, want)
}

// --- tailscale serve status --json parsing -----------------------------------------------------

// tailscaleStatusFixture mirrors the real `tailscale serve status --json` shape: a Web map keyed
// "<dnsname>:443" whose "/" handler proxies to our local port.
func tailscaleStatusFixture(dnsName string, localPort int) string {
	return `{
		"TCP": {"443": {"HTTPS": true}},
		"Web": {
			"` + dnsName + `:443": {
				"Handlers": {
					"/": {"Proxy": "http://127.0.0.1:` + itoa(localPort) + `"}
				}
			}
		}
	}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func TestParseTailscaleStatusFindsOurPort(t *testing.T) {
	data := []byte(tailscaleStatusFixture("my-machine.tailnetxyz.ts.net", 7780))
	dnsName, ok := ParseTailscaleStatus(data, 7780)
	if !ok {
		t.Fatal("ParseTailscaleStatus: want ok=true when 443 maps to our local port")
	}
	if dnsName != "my-machine.tailnetxyz.ts.net" {
		t.Fatalf("ParseTailscaleStatus dnsName = %q, want %q", dnsName, "my-machine.tailnetxyz.ts.net")
	}
}

func TestParseTailscaleStatusRejectsOtherPort(t *testing.T) {
	data := []byte(tailscaleStatusFixture("my-machine.tailnetxyz.ts.net", 9999))
	_, ok := ParseTailscaleStatus(data, 7780)
	if ok {
		t.Fatal("ParseTailscaleStatus: want ok=false when 443 maps to a different local port")
	}
}

func TestParseTailscaleStatusEmptyOrMalformed(t *testing.T) {
	if _, ok := ParseTailscaleStatus([]byte(`{}`), 7780); ok {
		t.Fatal("ParseTailscaleStatus({}): want ok=false")
	}
	if _, ok := ParseTailscaleStatus([]byte(`not json`), 7780); ok {
		t.Fatal("ParseTailscaleStatus(invalid json): want ok=false")
	}
}

// --- free port picking -----------------------------------------------------------------------

func TestFreePortSkipsOccupied(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:7780")
	if err != nil {
		t.Skipf("cannot occupy 7780 on this machine: %v", err)
	}
	defer ln.Close()

	port, err := FreePort(7780)
	if err != nil {
		t.Fatalf("FreePort: %v", err)
	}
	if port != 7781 {
		t.Fatalf("FreePort() = %d, want 7781 (7780 held by the test)", port)
	}
}

// --- code CLI resolution -----------------------------------------------------------------------

func TestResolveCodeBinaryMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	prevFallback := macFallback
	macFallback = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { macFallback = prevFallback })

	_, err := ResolveCodeBinary()
	if err == nil {
		t.Fatal("ResolveCodeBinary: want error when `code` is absent from PATH and the macOS app bundle")
	}
	if !containsSubstring(err.Error(), "code") {
		t.Fatalf("ResolveCodeBinary error %q does not mention the `code` CLI", err.Error())
	}
}

func containsSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// --- Running / Stop ------------------------------------------------------------------------

func TestRunningFalseOnStalePID(t *testing.T) {
	setup(t)
	st := State{PID: 999999999, Port: 7780}
	writeStateFixture(t, st)

	if _, ok := Running(); ok {
		t.Fatal("Running(): want false for a stale, non-existent pid")
	}
}

func TestStopDeadPIDRemovesStateFile(t *testing.T) {
	setup(t)
	writeStateFixture(t, State{PID: 999999999, Port: unusedPort(t)})

	if err := Stop(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Stop: err = %v, want ErrNotRunning", err)
	}
	if _, err := os.Stat(stateFile()); !os.IsNotExist(err) {
		t.Fatalf("Stop: vscode-serve.json still present (err=%v)", err)
	}
}

// unusedPort grabs an ephemeral port and releases it immediately, so nothing answers there.
func unusedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("unusedPort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func writeStateFixture(t *testing.T, st State) {
	t.Helper()
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state fixture: %v", err)
	}
	if err := os.MkdirAll(paths.StateDir(), 0o700); err != nil {
		t.Fatalf("mkdir StateDir: %v", err)
	}
	if err := os.WriteFile(stateFile(), data, 0o600); err != nil {
		t.Fatalf("write state fixture: %v", err)
	}
}

// --- failed-start regressions ----------------------------------------------------------------

func TestPrepareDataDirReportsUnreadableSettingsDir(t *testing.T) {
	setup(t)
	dir := filepath.Dir(MachineSettingsPath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := os.Stat(MachineSettingsPath()); err == nil || os.IsNotExist(err) {
		t.Skip("directory permissions not enforced (running as root?)")
	}

	notes := prepareDataDir()
	if !slices.ContainsFunc(notes, func(n string) bool { return strings.HasPrefix(n, "settings: ") }) {
		t.Fatalf("prepareDataDir notes = %q, want a settings note for the permission error", notes)
	}
	_ = os.Chmod(dir, 0o700)
	if _, err := os.Stat(MachineSettingsPath()); !os.IsNotExist(err) {
		t.Fatalf("settings.json exists after an unreadable-dir start (err=%v); seed/merge must be skipped", err)
	}
}

func TestStartTerminatesDaemonWhenStateWriteFails(t *testing.T) {
	setup(t)
	bin := t.TempDir()
	pidFile := filepath.Join(bin, "pid")
	script := "#!/bin/sh\necho $$ > " + pidFile + "\nexec sleep 300\n"
	if err := os.WriteFile(filepath.Join(bin, "code"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	// A directory at the state path makes the post-health writeState fail while Running() still reads "not running".
	if err := os.MkdirAll(filepath.Join(stateFile(), "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	port := unusedPort(t)
	// Answer on the port only once the fake daemon has spawned (after Start picked the port), standing in for serve-web.
	srv := &http.Server{ReadHeaderTimeout: time.Second}
	go func() {
		for {
			if _, err := os.Stat(pidFile); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			_ = srv.Serve(ln)
		}
	}()
	t.Cleanup(func() { _ = srv.Close() })

	if _, err := Start(context.Background(), Options{Port: port}); err == nil {
		t.Fatal("Start: want the state-write error")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("fake daemon never ran: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	for i := 0; i < 50 && liveProcess(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if liveProcess(pid) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		t.Fatalf("daemon pid %d still alive after a failed Start: unrecorded orphan", pid)
	}
}

// liveProcess is proc.Alive minus zombies (the test never reaps the daemon it spawned); uses ps so macOS works too.
func liveProcess(pid int) bool {
	if !proc.Alive(pid) {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
