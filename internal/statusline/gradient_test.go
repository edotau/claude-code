package statusline

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// easeAt keeps the ends fixed, leaves exp 0/1 linear, and moves the middle toward the end colors when exp < 1.
func TestEaseAt(t *testing.T) {
	for _, exp := range []float64{0, 1, 0.5} {
		if easeAt(0, exp) != 0 || easeAt(1, exp) != 1 {
			t.Errorf("exp %v must keep the ends at 0 and 1", exp)
		}
	}
	if easeAt(0.5, 0) != 0.5 || easeAt(0.5, 1) != 0.5 {
		t.Error("exp 0 and 1 must be linear")
	}
	if got := easeAt(0.25, 0.5); got != 0.5 {
		t.Errorf("easeAt(0.25, 0.5) = %v, want 0.5", got)
	}
}

func gradientPayload(cwd string) string {
	return `{"cwd":"` + cwd + `","model":{"id":"claude-opus-5-5","display_name":"Opus 5.5"},` +
		`"context_window":{"used_percentage":31,"context_window_size":1000000,"total_input_tokens":309500},` +
		`"rate_limits":{"seven_day":{"used_percentage":15}}}`
}

// The default gradient line: twoline's ctx block, the display name, no 7d/total/branch, velocity only with changes.
func TestGradientLine(t *testing.T) {
	hermetic(t)
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.email=a@b", "-c", "user.name=x"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "f")
	git("commit", "-qm", "f")
	const ctx = "Opus 5.5 · ctx ━━━─────── 31.0% · 309.5k/1m"
	if got := plain(Render([]byte(gradientPayload(repo)), "gradient")); !strings.HasSuffix(got, ctx) {
		t.Errorf("clean tree: %q must end %q", got, ctx)
	}
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("a\nc\nd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Dir(stampFile("velocity", "x"))) // drop the clean tree's cached velocity
	if got := plain(Render([]byte(gradientPayload(repo)), "gradient")); !strings.HasSuffix(got, ctx+" | +2/-1") {
		t.Errorf("dirty tree: %q must end %q", got, ctx+" | +2/-1")
	}
}
