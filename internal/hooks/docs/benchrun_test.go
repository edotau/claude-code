package docs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

const cannedBench = `{"noisy_load":false,"packages":[{"pkg":"internal/memory","benchmarks":[
{"name":"PromptRecall/bank-8","deltas":{"allocs/op":{"change":"-12.00%","p":"p=0.000 n=5","significant":true},
"sec/op":{"change":"-3.00%","p":"p=0.010 n=5","significant":true},"B/op":{"change":"~","p":"p=1.000","significant":false}}},
{"name":"Noisy-8","flags":["noisy"],"deltas":{"sec/op":{"change":"+9.00%","p":"p=0.020","significant":true}}},
{"name":"Still-8","deltas":{"sec/op":{"change":"~","p":"p=1.000","significant":false}}}]}]}`

func TestBenchNoticeLines(t *testing.T) {
	got := benchNoticeLines([]byte(cannedBench))
	want := []string{
		"bench internal/memory: PromptRecall/bank allocs -12.00% (p=0.000); ns -3.00% (p=0.010)",
		"bench internal/memory: Noisy ns not judged (noisy)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines = %q, want %q", got, want)
	}
	loaded := strings.Replace(cannedBench, `"noisy_load":false`, `"noisy_load":true`, 1)
	if l := benchNoticeLines([]byte(loaded)); !strings.Contains(l[0], "allocs -12.00% (p=0.000); ns not judged (load)") {
		t.Errorf("loaded = %q", l)
	}
}

func benchRepo(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	pkg := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, body := range map[string]string{"x.go": "package pkg\n", "x_test.go": "package pkg\nimport \"testing\"\nfunc BenchmarkX(b *testing.B) {}\n"} {
		if err := os.WriteFile(filepath.Join(pkg, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "pkg"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x"}} {
		if out, err := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	out, _ := exec.CommandContext(context.Background(), "git", "-C", dir, "rev-parse", "HEAD").Output()
	return dir, strings.TrimSpace(string(out))
}

func TestBenchOnCommitSkipRules(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bench post-commit runs are unix only")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir, sha := benchRepo(t)
	var spawned [][]string
	origSpawn, origLoad := benchSpawn, benchLoad
	t.Cleanup(func() { benchSpawn, benchLoad = origSpawn, origLoad })
	benchSpawn = func(args []string, _ string) int { spawned = append(spawned, args); return 1 }
	load := 0.0
	benchLoad = func(context.Context) float64 { return load }
	in := core.ParseInput(strings.NewReader(`{"session_id":"s1","tool_name":"Bash"}`))
	leaf := core.ParseInput(strings.NewReader(`{"session_id":"s1","agent_id":"a1","tool_name":"Bash"}`))

	benchOnCommit(context.Background(), leaf, dir, sha)
	load = 1e6
	benchOnCommit(context.Background(), in, dir, sha[:7]) // busy: records the skip, spawns nothing
	load = 0
	benchOnCommit(context.Background(), in, dir, sha[:7]) // same commit already recorded
	if len(spawned) != 0 {
		t.Fatalf("spawned %v, want none (leaf, busy, repeat)", spawned)
	}
	marker, _ := filepath.Glob(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "state", "harness", "bench", "post-commit", "*.marker"))
	if raw, _ := os.ReadFile(marker[0]); !strings.HasPrefix(string(raw), "skipped: busy") {
		t.Errorf("marker = %q", raw)
	}
	_ = os.Remove(marker[0])
	benchOnCommit(context.Background(), in, dir, sha[:7])
	if len(spawned) != 1 || !strings.Contains(strings.Join(spawned[0], " "), "--path "+filepath.Join("pkg", "x.go")) {
		t.Errorf("spawned = %v, want one run for pkg/x.go", spawned)
	}
}

func TestBenchCommitFormsAndLaggingHead(t *testing.T) {
	for _, cmd := range []string{"rtk git add a && rtk git commit -m x", "git -C /r commit -qm x", "claude-code worktree commit --task p 1 -m x f.go"} {
		if !tickMatch.MatchString(cmd) {
			t.Errorf("tickMatch missed %q", cmd)
		}
	}
	if runtime.GOOS == "windows" {
		t.Skip("bench post-commit runs are unix only")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir, sha := benchRepo(t)
	if out, err := exec.CommandContext(context.Background(), "git", "-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "y").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	var spawned int
	origSpawn, origLoad := benchSpawn, benchLoad
	t.Cleanup(func() { benchSpawn, benchLoad = origSpawn, origLoad })
	benchSpawn = func([]string, string) int { spawned++; return 1 }
	benchLoad = func(context.Context) float64 { return 0 }
	in := core.ParseInput(strings.NewReader(`{"session_id":"s1","tool_name":"Bash"}`))
	benchOnCommit(context.Background(), in, dir, sha[:7]) // HEAD is already one commit past sha
	if spawned != 1 {
		t.Errorf("spawned %d runs for a commit HEAD has moved past, want 1", spawned)
	}
}
