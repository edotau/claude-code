package inspect

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/worktree"
)

// fixFake swaps the exec seam: gofmt -w and mdsmith fix append a marker to each file; the listers name every file.
type fixFake struct{ calls []string }

func (f *fixFake) install(t *testing.T) {
	t.Helper()
	origRun, origLook := gateRun, gateLookPath
	t.Cleanup(func() { gateRun, gateLookPath = origRun, origLook })
	gateLookPath = func(p string) (string, error) {
		if strings.Contains(p, "/") {
			return "", exec.ErrNotFound
		}
		return "/bin/" + p, nil
	}
	gateRun = func(_ context.Context, dir, name string, args ...string) (string, error) {
		f.calls = append(f.calls, name+" "+strings.Join(args, " "))
		switch {
		case name == "gofmt" && args[0] == "-l":
			return strings.Join(args[1:], "\n") + "\n", nil
		case name == "mdsmith" && args[0] == "check":
			return "m.md:1:1 MDS008 blank lines\n", nil
		case name == "gofmt", name == "mdsmith" && args[0] == "fix":
			for _, a := range args {
				if !strings.HasPrefix(a, "-") && a != "fix" {
					appendTo(dir, a)
				}
			}
		}
		return "", nil
	}
}

func appendTo(dir, rel string) {
	p := filepath.Join(dir, rel)
	b, _ := os.ReadFile(p)
	_ = os.WriteFile(p, append(b, "// fixed\n"...), 0o644)
}

func fixRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(proc.DispatchIDEnv, "")
	t.Setenv(proc.DispatchDepthEnv, "")
	dir := paths.Real(t.TempDir())
	repoGit(t, dir, "init", "-q")
	for _, f := range []string{"a.go", "b.go", "m.md"} {
		writeRepoFile(t, dir, f, "x\n")
	}
	repoGit(t, dir, "add", "-A")
	repoGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func runFix(dir string, dry bool, sel ...string) (code int, out, errOut string) {
	var so, se bytes.Buffer
	code = Fix(FixOptions{Dir: dir, Paths: sel, DryRun: dry, Out: &so, Err: &se, Source: worktree.ClaimSource{
		Alive: func(int) bool { return true }, Self: "me", Owner: func() int { return 0 },
		Label: func(pid int) string { return fmt.Sprintf("session peer (pid %d)", pid) }}})
	return code, so.String(), se.String()
}

func TestFixOnlyNamedFiles(t *testing.T) {
	dir := fixRepo(t)
	f := &fixFake{}
	f.install(t)
	code, out, _ := runFix(dir, true, "a.go", "m.md")
	if code != 0 || !strings.Contains(out, "a.go: gofmt") || !strings.Contains(out, "m.md: mdsmith") {
		t.Fatalf("dry run: code %d, out %q", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.go")); string(b) != "x\n" {
		t.Fatalf("dry run changed a.go: %q", b)
	}
	code, out, _ = runFix(dir, false, "a.go", "m.md")
	if code != 0 || !strings.Contains(out, "a.go: gofmt\n") || !strings.Contains(out, "m.md: mdsmith\n") || !strings.Contains(out, "2 file(s) changed") {
		t.Fatalf("real run: code %d, out %q", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "b.go")); string(b) != "x\n" {
		t.Fatalf("unnamed b.go changed: %q", b)
	}
	if c := strings.Join(f.calls, "\n"); strings.Contains(c, "b.go") {
		t.Fatalf("a tool was handed b.go:\n%s", c)
	}
}

func TestFixRefusesWithoutPath(t *testing.T) {
	dir := fixRepo(t)
	f := &fixFake{}
	f.install(t)
	code, _, errOut := runFix(dir, false)
	if code != FixExitUsage || !strings.Contains(errOut, "--path") || len(f.calls) != 0 {
		t.Fatalf("code %d, err %q, calls %v", code, errOut, f.calls)
	}
}

func TestFixSkipsHeldAndUntrackedFiles(t *testing.T) {
	dir := fixRepo(t)
	f := &fixFake{}
	f.install(t)
	writeRepoFile(t, dir, "a.go", "edited\n")
	writeRepoFile(t, dir, "new.go", "x\n")
	if err := os.MkdirAll(worktree.ClaimsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"schema":1,"path":%q,"ts":%d}`+"\n", filepath.Join(dir, "a.go"), time.Now().Unix())
	if err := os.WriteFile(filepath.Join(worktree.ClaimsDir(), "4242-peer.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runFix(dir, false, "a.go", "b.go", "new.go")
	if code != FixExitSkipped || !strings.Contains(errOut, "a.go: skipped, held by session peer") || !strings.Contains(errOut, "new.go: skipped, not tracked") {
		t.Fatalf("code %d, err %q", code, errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.go")); string(b) != "edited\n" {
		t.Fatalf("held a.go was rewritten: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "b.go")); string(b) != "x\n// fixed\n" {
		t.Fatalf("b.go was not fixed: %q", b)
	}
}
