package pi

import (
	"os"
	"runtime"
	"strings"
	"testing"
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
	for _, want := range []string{"safety-scan", "permission-gate", "harness_agent"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("extension lacks %q", want)
		}
	}
	if strings.Contains(string(body), "__BIN__") || strings.Contains(string(body), "__MODULES__") {
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

func TestGuardRefusesNoModules(t *testing.T) {
	if _, err := renderGuard("/bin/x", nil); err == nil {
		t.Fatal("rendered an extension with no guard modules")
	}
}
