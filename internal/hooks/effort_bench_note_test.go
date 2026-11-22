package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeBenchRepoFixture(t testing.TB) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	benched := filepath.Join(repo, "internal", "alpha", "sub")
	plain := filepath.Join(repo, "internal", "beta")
	for _, d := range []string{benched, plain} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(benched, "sub.go"):      "package sub\n",
		filepath.Join(benched, "sub_test.go"): "package sub\nimport \"testing\"\nfunc BenchmarkSub(b *testing.B) {}\n",
		filepath.Join(plain, "beta.go"):       "package beta\n",
		filepath.Join(plain, "beta_test.go"):  "package beta\nimport \"testing\"\nfunc TestBeta(t *testing.T) {}\n",
	}
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func TestEffortBenchNoteOnlyForBenchedPackage(t *testing.T) {
	effortHome(t)
	stubLabelModels(t, "")
	repo := writeBenchRepoFixture(t)
	want := "perf: internal/alpha/sub has benchmarks; run rtk bypass go test -run ^$ -bench . -benchmem -count=10 " +
		"./internal/alpha/sub/ before and after, compare with benchstat against state/harness/bench/alpha-sub/rolling.txt, and report the table"

	for _, header := range []string{
		`Owned: ["internal/alpha/sub/sub.go", "internal/beta/beta.go"]`,
		"Owned files: internal/alpha/sub/sub.go, internal/beta/beta.go.",
		"owns: internal/alpha/sub/sub*.go and internal/beta/beta.go — do work",
	} {
		prompt := header + "\n\nTask: update sub and beta"
		payload := fmt.Sprintf(`{"cwd":%q,"tool_name":"Agent","tool_input":{"subagent_type":"general-purpose","model":"sonnet","prompt":%q,"description":"sonnet: update sub"}}`,
			repo, prompt)
		res := effortResult(payload)
		if res.Decision != 0 || res.UpdatedInput != nil {
			t.Fatalf("%q: decision=%v updatedInput=%v, want context-only allow", header, res.Decision, res.UpdatedInput)
		}
		if res.Context != want {
			t.Fatalf("%q: context = %q, want %q", header, res.Context, want)
		}
	}

	// Modify the plain package's test file to add a benchmark and verify the mtime cache invalidates.
	plainTest := filepath.Join(repo, "internal", "beta", "beta_test.go")
	later := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(plainTest, []byte("package beta\nimport \"testing\"\nfunc BenchmarkBeta(b *testing.B) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(plainTest, later, later); err != nil {
		t.Fatal(err)
	}
	got := benchPackages(repo, "Owned: internal/beta/beta.go")
	if len(got) != 1 || got[0] != "internal/beta" {
		t.Fatalf("after adding BenchmarkBeta: benchPackages = %v, want [internal/beta]", got)
	}
}

func BenchmarkBenchNoteWarm(b *testing.B) {
	repo := writeBenchRepoFixture(b)
	task := "Owned: internal/alpha/sub/sub.go, internal/beta/beta.go\n\nTask: update sub"
	e := &effortPlan{mode: modeToday}
	if got := e.benchNote(repo, task); !strings.Contains(got, "perf: internal/alpha/sub has benchmarks;") {
		b.Fatalf("warm setup got %q", got)
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = e.benchNote(repo, task)
	}
}
