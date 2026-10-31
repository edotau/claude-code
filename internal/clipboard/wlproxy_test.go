package clipboard

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const chainParent = ": xclip -selection clipboard wl-paste --type image/bmp"

// fakeReal writes a real-wl-paste stand-in that logs its args and offers a BMP, plus a PNG when withPNG.
func fakeReal(t *testing.T, withPNG bool) (real string, log func() []string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "log")
	if err := os.WriteFile(filepath.Join(dir, "bmp"), bitfieldsBMP(), 0o644); err != nil {
		t.Fatal(err)
	}
	if withPNG {
		if err := os.WriteFile(filepath.Join(dir, "png"), pngBytes(t), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	body := "#!/bin/sh\necho \"$*\" >> " + logPath + "\ncase \"$*\" in\n*image/png*) cat " + dir + "/png;;\n*image/bmp*) cat " + dir + "/bmp;;\nesac\n"
	real = filepath.Join(dir, "wl-paste")
	if err := os.WriteFile(real, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return real, func() []string {
		b, _ := os.ReadFile(logPath)
		return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", ";"))
	}
}

func TestProxyRouteTable(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "")
	bmp, png2 := []string{"--type", "image/bmp"}, []string{"--type", "image/png"}
	cases := []struct {
		name, mode, parent string
		args               []string
		withPNG, wantOK    bool
		wantCode           int
		kind               string // bmp24 | png | raw | none
		wantLog            []string
	}{
		{"chain bmp", "", chainParent, bmp, false, true, 0, "bmp24", []string{"--type", "image/bmp"}},
		{"chain png from bmp-only real", "", chainParent, []string{"-t", "image/png"}, false, true, 0, "png", []string{"-t", "image/png;--type", "image/bmp"}},
		{"chain png genuine real", "", chainParent, png2, true, true, 0, "raw", []string{"--type", "image/png"}},
		{"plain parent", "", "bash", bmp, false, false, 0, "none", nil},
		{"wl-paste leg without xclip leg", "", "sh -c wl-paste --type image/bmp", bmp, false, false, 0, "none", nil},
		{"opencode png", "", "opencode", png2, false, true, 0, "png", []string{"--type", "image/png;--type", "image/bmp"}},
		{"opencode bmp", "", "opencode", bmp, false, false, 0, "none", nil},
		{"gemini png", "", "node /x/gemini", png2, false, true, 0, "png", []string{"--type", "image/png;--type", "image/bmp"}},
		{"gemini bmp", "", "node /x/gemini", bmp, false, false, 0, "none", nil},
		{"mode 0", "0", chainParent, bmp, false, false, 0, "none", nil},
		{"mode 1 no parent", "1", "", bmp, false, true, 0, "bmp24", []string{"--type", "image/bmp"}},
		{"reject bmp", "reject", chainParent, bmp, false, true, 1, "none", nil},
		{"reject png", "reject", chainParent, png2, false, false, 0, "none", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			real, log := fakeReal(t, tc.withPNG)
			var out, errb bytes.Buffer
			c := proxyCall{real: real, args: tc.args, env: os.Environ(), stdout: &out, stderr: &errb}
			code, ok := c.route(tc.mode, tc.parent)
			if code != tc.wantCode || ok != tc.wantOK {
				t.Fatalf("route = (%d, %v), want (%d, %v); stderr %q", code, ok, tc.wantCode, tc.wantOK, errb.String())
			}
			checkProxyOut(t, tc.kind, out.Bytes())
			if got := strings.Join(log(), " "); got != strings.Join(tc.wantLog, " ") {
				t.Errorf("real wl-paste saw %q, want %q", got, tc.wantLog)
			}
		})
	}
}

func checkProxyOut(t *testing.T, kind string, out []byte) {
	t.Helper()
	switch kind {
	case "bmp24":
		if len(out) < 30 || string(out[:2]) != "BM" || binary.LittleEndian.Uint16(out[28:]) != 24 {
			t.Errorf("want a 24bpp BMP, got %d bytes", len(out))
		}
	case "png", "raw":
		if _, err := png.DecodeConfig(bytes.NewReader(out)); err != nil {
			t.Errorf("want a PNG: %v", err)
		}
		if kind == "raw" && !bytes.Equal(out, pngBytes(t)) {
			t.Error("genuine PNG was altered")
		}
	default:
		if len(out) != 0 {
			t.Errorf("want no output, got %d bytes", len(out))
		}
	}
}

func TestProxySSHLegs(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "x")
	t.Setenv("USER", "wlproxy-no-such-user")
	for _, tc := range []struct {
		mime       string
		wantOK     bool
		wantStderr string
	}{{"image/bmp", true, ""}, {"image/png", true, "bridge"}, {"text/plain", false, ""}} {
		t.Run(tc.mime, func(t *testing.T) {
			real, log := fakeReal(t, false)
			var out, errb bytes.Buffer
			c := proxyCall{real: real, args: []string{"--type", tc.mime}, env: os.Environ(), stdout: &out, stderr: &errb}
			code, ok := c.route("1", "")
			if code != 0 || ok != tc.wantOK || out.Len() != 0 {
				t.Fatalf("route = (%d, %v), %d bytes; want (0, %v), empty", code, ok, out.Len(), tc.wantOK)
			}
			if !strings.Contains(errb.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want it to mention %q", errb.String(), tc.wantStderr)
			}
			if len(log()) != 0 {
				t.Errorf("real wl-paste ran under SSH: %v", log())
			}
		})
	}
}

func TestProxyEnvEmptyWaylandDisplayReplaced(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	if !isSocket(wslgSocket) { // wslgSocket is a const: without WSLg on this host the replace path cannot run
		t.Skip("no WSLg socket on this host")
	}
	n := 0
	for _, kv := range proxyEnv() {
		if strings.HasPrefix(kv, "WAYLAND_DISPLAY=") {
			n++
			if kv != "WAYLAND_DISPLAY="+wslgSocket {
				t.Errorf("got %q, want the WSLg socket", kv)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d WAYLAND_DISPLAY entries, want exactly 1", n)
	}
}

func TestFindRealWlPasteSkipsProxies(t *testing.T) {
	stale := []byte("#!/bin/sh\n# " + proxyMarker + " (v0)\n")
	foreign := []byte("#!/bin/sh\necho real\n")
	var dirs []string
	for _, f := range []struct {
		body []byte
		mode os.FileMode
	}{{ProxyScript, 0o755}, {stale, 0o755}, {foreign, 0o644}, {foreign, 0o755}, {foreign, 0o755}} {
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, "wl-paste"), f.body, f.mode); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, d)
	}
	t.Setenv("PATH", strings.Join(dirs, ":"))
	if got, want := findRealWlPaste(), filepath.Join(dirs[3], "wl-paste"); got != want {
		t.Errorf("findRealWlPaste = %q, want %q", got, want)
	}
	t.Setenv("PATH", strings.Join(dirs[:3], ":"))
	if got := findRealWlPaste(); got != "" {
		t.Errorf("only proxies and a non-executable on PATH: got %q, want none", got)
	}
}

func TestProxyShimForwardsArgs(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".claude", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "claude-code"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(t.TempDir(), "wl-paste")
	if err := os.WriteFile(shim, ProxyScript, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shim, "--type", "image/png", "--help")
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "wl-paste\n--type\nimage/png\n--help\n"; got != want {
		t.Errorf("shim passed %q, want %q", got, want)
	}
}

// withProcVersion points procVersionPath at a fixture file containing content, restoring on cleanup.
func withProcVersion(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "version")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := procVersionPath
	procVersionPath = path
	oldOS := doctorGOOS
	doctorGOOS = "linux" // the /proc/version fixture models a Linux host, whatever runs the test
	t.Cleanup(func() { procVersionPath, doctorGOOS = old, oldOS })
}

func TestOnWSLKernel(t *testing.T) {
	withProcVersion(t, "Linux version 5.15.90.1-microsoft-standard-WSL2 (...)\n")
	if !OnWSLKernel() {
		t.Error("microsoft kernel string: want WSL detected")
	}
	withProcVersion(t, "Linux version 6.8.0-generic (buildd@host) ...\n")
	if OnWSLKernel() {
		t.Error("plain Linux kernel string: want WSL NOT detected")
	}
}

func TestOnWSLKernelMissingFile(t *testing.T) {
	old := procVersionPath
	procVersionPath = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { procVersionPath = old })
	if OnWSLKernel() {
		t.Error("unreadable /proc/version: want WSL NOT detected")
	}
}

// setHome points UserHomeDir (via $HOME) at a scratch dir so ProxyPath never touches the real home.
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestInstallProxyAtAbsent(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "sub", "wl-paste")
	var out bytes.Buffer
	if err := installProxyAt(&out, dest, false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("proxy not written: %v", err)
	}
	if !bytes.Equal(b, ProxyScript) {
		t.Error("written proxy does not match embedded script")
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
	if !strings.Contains(out.String(), "wrote") {
		t.Errorf("output = %q, want a wrote line", out.String())
	}
}

func TestInstallProxyAtCurrent(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "wl-paste")
	if err := os.WriteFile(dest, ProxyScript, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := installProxyAt(&out, dest, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dest)
	if !bytes.Equal(b, ProxyScript) {
		t.Error("current proxy was rewritten")
	}
	if !strings.Contains(out.String(), "current") {
		t.Errorf("output = %q, want a current line", out.String())
	}
}

func TestInstallProxyAtStaleIsUpdated(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "wl-paste")
	stale := []byte("#!/usr/bin/env bash\n# " + proxyMarker + " (v0, old copy)\necho old\n")
	if err := os.WriteFile(dest, stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := proxyState(dest); got != ProxyStale {
		t.Fatalf("fixture precondition: proxyState = %v, want ProxyStale", got)
	}
	var out bytes.Buffer
	if err := installProxyAt(&out, dest, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dest)
	if !bytes.Equal(b, ProxyScript) {
		t.Error("stale proxy was not updated to the current embedded script")
	}
	if !strings.Contains(out.String(), "wrote") {
		t.Errorf("output = %q, want a wrote line", out.String())
	}
}

func TestInstallProxyAtForeignUntouched(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "wl-paste")
	foreign := []byte("#!/bin/sh\nexec /usr/bin/wl-paste.real \"$@\"\n")
	if err := os.WriteFile(dest, foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	// Teeth: grep the fixture file for OUR marker before asserting non-overwrite — proves the
	// precondition (a genuinely foreign file, not an accidental copy of the proxy) actually holds.
	if bytes.Contains(before, []byte(proxyMarker)) {
		t.Fatal("fixture precondition: foreign file unexpectedly contains the proxy marker")
	}
	var out bytes.Buffer
	if err := installProxyAt(&out, dest, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("foreign file was modified: before %q, after %q", before, after)
	}
	if !strings.Contains(out.String(), "skip") || !strings.Contains(out.String(), "not the claude-code proxy") {
		t.Errorf("output = %q, want a skip line naming the foreign file", out.String())
	}
}

func TestInstallProxyAtDryRunDoesNotWrite(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "wl-paste")
	var out bytes.Buffer
	if err := installProxyAt(&out, dest, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("--dry-run wrote the proxy file")
	}
	if !strings.Contains(out.String(), "would write") {
		t.Errorf("output = %q, want a would-write line", out.String())
	}
}

func TestInstallProxyWSLGate(t *testing.T) {
	home := setHome(t)
	dest := filepath.Join(home, ".local", "bin", "wl-paste")

	withProcVersion(t, "Linux version 6.8.0-generic (buildd@host) ...\n")
	var offWSL bytes.Buffer
	if err := InstallProxy(&offWSL, false); err != nil {
		t.Fatal(err)
	}
	if offWSL.String() != "" {
		t.Errorf("off-WSL output = %q, want silent no-op", offWSL.String())
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("off-WSL InstallProxy wrote the proxy file")
	}

	withProcVersion(t, "Linux version 5.15.90.1-microsoft-standard-WSL2 (...)\n")
	var onWSL bytes.Buffer
	if err := InstallProxy(&onWSL, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(onWSL.String(), "wrote") {
		t.Errorf("WSL output = %q, want a wrote line", onWSL.String())
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("WSL InstallProxy did not write the proxy: %v", err)
	}
	if !bytes.Equal(b, ProxyScript) {
		t.Error("written proxy does not match embedded script")
	}

	// Re-run: now current, reported as such, left byte-identical.
	var again bytes.Buffer
	if err := InstallProxy(&again, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(again.String(), "current") {
		t.Errorf("re-run output = %q, want a current line", again.String())
	}
}

func TestDoctorDetailOffWSL(t *testing.T) {
	setHome(t)
	withProcVersion(t, "Linux version 6.8.0-generic (buildd@host) ...\n")
	ok, detail := DoctorDetail()
	if !ok {
		t.Errorf("off-WSL: ok = false, detail %q", detail)
	}
	if !strings.Contains(detail, "not WSL") {
		t.Errorf("detail = %q, want it to say not WSL", detail)
	}
}

func TestDoctorDetailMissingOnWSL(t *testing.T) {
	setHome(t)
	withProcVersion(t, "Linux version 5.15.90.1-microsoft-standard-WSL2 (...)\n")
	ok, detail := DoctorDetail()
	if ok {
		t.Errorf("missing proxy on WSL: ok = true, detail %q", detail)
	}
	if !strings.Contains(detail, "claude-code install") {
		t.Errorf("detail = %q, want it to name claude-code install", detail)
	}
}

func TestDoctorDetailCurrentOnWSL(t *testing.T) {
	home := setHome(t)
	withProcVersion(t, "Linux version 5.15.90.1-microsoft-standard-WSL2 (...)\n")
	dest := filepath.Join(home, ".local", "bin", "wl-paste")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, ProxyScript, 0o755); err != nil {
		t.Fatal(err)
	}
	// PATH scoped to dest's own dir only — a real machine's wl-paste elsewhere on PATH must not
	// make exec.LookPath resolve ahead of the fixture and spuriously flip this to "not ok".
	t.Setenv("PATH", filepath.Dir(dest))
	ok, detail := DoctorDetail()
	if !ok {
		t.Errorf("current proxy on WSL: ok = false, detail %q", detail)
	}
	if !strings.Contains(detail, "claude-code clip") {
		t.Errorf("detail = %q, want it to name claude-code clip as the fallback", detail)
	}
}
