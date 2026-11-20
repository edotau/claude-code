package pi

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/tools"
)

func TestGuardWritesPrivateExtension(t *testing.T) {
	dir := t.TempDir()
	g, err := WriteGuard(dir)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(g.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--pi-guard", "HARNESS_HOOK_", "VERIFIED_PI", "registerTool", "tool_call"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("extension lacks %q", want)
		}
	}
	if strings.Contains(string(body), "__BIN__") || strings.Contains(string(body), "__PI_VERSION__") || strings.Contains(string(body), "__BUDGET_MS__") {
		t.Error("placeholder left unrendered")
	}
	if info, _ := os.Stat(g.Path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestGuardArgvAndEnv(t *testing.T) {
	g, err := WriteGuard(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a := g.Argv(); len(a) != 2 || a[0] != "-e" || a[1] != g.Path {
		t.Errorf("argv = %v", a)
	}
	if e := strings.Join(g.Env(), " "); !strings.Contains(e, nonceFileEnv+"="+g.NonceFile) || !strings.Contains(e, nonceValueEnv+"=") {
		t.Errorf("env = %v", g.Env())
	}
}

func TestGuardReadyNeedsTheNonce(t *testing.T) {
	g, err := WriteGuard(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if g.Ready() == nil {
		t.Fatal("ready before the extension wrote the nonce")
	}
	if err := os.WriteFile(g.NonceFile, []byte("stale"), 0o600); err != nil || g.Ready() == nil {
		t.Fatal("a wrong nonce must not be ready")
	}
	if err := os.WriteFile(g.NonceFile, []byte(g.Nonce), 0o600); err != nil || g.Ready() != nil {
		t.Fatalf("right nonce not ready: %v", g.Ready())
	}
}

func TestGuardPinsVerifiedPi(t *testing.T) {
	body, err := renderGuard("/bin/x", 7000)
	if err != nil || !strings.Contains(string(body), "VERIFIED_PI: string = "+strconv.Quote(tools.PiPin)) {
		t.Fatalf("guard does not carry the install pin %s: %v", tools.PiPin, err)
	}
}
