package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/clipboard"
)

// stubXclip fakes xclip over a file clipboard: -o prints it, -i replaces it with stdin.
func stubXclip(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	clip := filepath.Join(dir, "clip")
	if err := os.WriteFile(clip, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$*\" in *-o*) /bin/cat '" + clip + "';; *) /bin/cat > '" + clip + "';; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "xclip"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Cleanup(clipboard.SetWSLForTest(false)) // on a WSL host the real Windows clipboard would win over the stub
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", ":0")
	return clip
}

func runClipClear(t *testing.T, stdin string) int {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(stdin); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close() })
	return cmdClipClear([]string{"--after", "10ms"})
}

func TestClipClearClearsOnlyOnMatch(t *testing.T) {
	clip := stubXclip(t, "tok-123")
	if code := runClipClear(t, clipboard.Digest("other")+"\n"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if b, _ := os.ReadFile(clip); string(b) != "tok-123" {
		t.Fatalf("mismatch must not clear, clipboard = %q", b)
	}
	if code := runClipClear(t, clipboard.Digest("tok-123")+"\n"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if b, _ := os.ReadFile(clip); strings.TrimSpace(string(b)) != "" {
		t.Fatalf("match must clear, clipboard = %q", b)
	}
}

func TestClipClearNoDigest(t *testing.T) {
	stubXclip(t, "x")
	if code := runClipClear(t, "  \n"); code == 0 {
		t.Fatal("empty stdin must fail")
	}
}
