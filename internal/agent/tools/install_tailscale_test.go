package tools

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallTailscaleDryRunPerOS(t *testing.T) {
	setupToolsTestEnv(t)
	old := goosOverride
	t.Cleanup(func() { goosOverride = old })
	for goos, want := range map[string]string{
		"darwin": "brew install --cask tailscale-app",
		"linux":  tailscaleInstallScript,
	} {
		goosOverride = goos
		var buf bytes.Buffer
		if err := installTailscale(context.Background(), &buf, t.TempDir(), true); err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		if !strings.Contains(buf.String(), want) {
			t.Errorf("%s output = %q, want %q", goos, buf.String(), want)
		}
	}
	goosOverride = "windows"
	if err := installTailscale(context.Background(), &bytes.Buffer{}, "", true); err == nil || !strings.Contains(err.Error(), "tailscale.com/download") {
		t.Errorf("windows err = %v, want download URL", err)
	}
}

// The macOS app puts no tailscale on PATH: the wrapper must run the app's CLI with the caller's args, and an existing file is kept.
func TestLinkTailscaleCLIWrapsAppBinary(t *testing.T) {
	setupToolsTestEnv(t)
	app := filepath.Join(t.TempDir(), "Tailscale")
	if err := os.WriteFile(app, []byte("#!/bin/sh\necho app \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := tailscaleAppBin
	tailscaleAppBin = app
	t.Cleanup(func() { tailscaleAppBin = old })
	if err := linkTailscaleCLI(&bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(localBinDir(), "tailscale")
	out, err := exec.Command(dst, "status", "--json").Output()
	if err != nil || string(out) != "app status --json\n" {
		t.Fatalf("wrapper ran %q err=%v, want the app CLI with the args", out, err)
	}
	var buf bytes.Buffer
	if err := linkTailscaleCLI(&buf, false); err != nil || !strings.Contains(buf.String(), "keep") {
		t.Fatalf("second run = %q err=%v, want keep", buf.String(), err)
	}
}

func TestTailscaleRepairsDanglingCLI(t *testing.T) {
	setupToolsTestEnv(t)
	app := filepath.Join(t.TempDir(), "Tailscale")
	writeToolsFixture(t, app, "#!/bin/sh\nprintf 'app %s\\n' \"$1\"\n", 0o755)
	old := tailscaleAppBin
	tailscaleAppBin = app
	t.Cleanup(func() { tailscaleAppBin = old })
	dst := filepath.Join(localBinDir(), "tailscale")
	symlinkToolsFixture(t, filepath.Join(t.TempDir(), "missing"), dst)

	var out bytes.Buffer
	if err := linkTailscaleCLI(&out, false); err != nil {
		t.Fatal(err)
	}
	got, err := exec.Command(dst, "status").Output()
	if err != nil || string(got) != "app status\n" {
		t.Fatalf("repaired CLI output = %q, error = %v; want app status", got, err)
	}
	if strings.Contains(out.String(), "keep") {
		t.Fatalf("dangling CLI was reported as kept: %s", &out)
	}
}
