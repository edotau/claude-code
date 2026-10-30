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
