package inspect

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
)

func stampKey(t *testing.T, dir string, pkgs ...string) string {
	t.Helper()
	key, err := ComputeStampKey(dir, pkgs, DefaultStampGates)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func stampRepo(t *testing.T) string {
	t.Helper()
	dir := newDiffRepo(t)
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\nimport _ \"x/dep\"\nfunc P() int { return 2 }\n")
	writeRepoFile(t, dir, "dep/d.go", "package dep\nimport _ \"x/transitive\"\n")
	writeRepoFile(t, dir, "transitive/t.go", "package transitive\nimport _ \"embed\"\n//go:embed data/*\nvar data string\n")
	writeRepoFile(t, dir, "transitive/data/a.txt", "one\n")
	writeRepoFile(t, dir, "go.sum", "sum\n")
	writeRepoFile(t, dir, "pkg/testdata/nested/expected.json", "{}\n")
	writeRepoFile(t, dir, "dep/testdata/input.txt", "input\n")
	for _, name := range []string{".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json", "Makefile"} {
		writeRepoFile(t, dir, name, "config\n")
	}
	return dir
}

func TestStampDirtyCleanAndCommitBlobs(t *testing.T) {
	dir := stampRepo(t)
	before := stampKey(t, dir, "./pkg")
	repoGit(t, dir, "add", "-A")
	repoGit(t, dir, "commit", "-q", "-m", "change")
	sha, err := proc.RunGit(context.Background(), dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	committed, err := ComputeCommitStampKey(context.Background(), dir, strings.TrimSpace(sha), []string{"pkg"}, DefaultStampGates)
	if err != nil || before != committed || before != stampKey(t, dir, "pkg") {
		t.Fatalf("dirty %s committed %s error %v", before, committed, err)
	}
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\nimport _ \"x/dep\"\nfunc P() int { return 3 }\n")
	if before == stampKey(t, dir, "pkg") {
		t.Fatal("one-byte edit reused the stamp key")
	}
	again, err := ComputeCommitStampKey(context.Background(), dir, strings.TrimSpace(sha), []string{"pkg"}, DefaultStampGates)
	if err != nil || again != committed {
		t.Fatalf("commit key used working tree contents: %s %v", again, err)
	}
}

func TestStampInputsInvalidate(t *testing.T) {
	dir := stampRepo(t)
	before := stampKey(t, dir, "pkg")
	for _, name := range []string{"pkg/p_test.go", "dep/d.go", "transitive/t.go", "transitive/data/a.txt", "go.mod", "go.sum",
		"pkg/testdata/nested/expected.json", "dep/testdata/input.txt", ".golangci.yml", ".golangci.yaml", ".golangci.toml",
		".golangci.json", "Makefile"} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, name)
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			writeRepoFile(t, dir, name, string(b)+" ")
			if got := stampKey(t, dir, "pkg"); got == before {
				t.Fatalf("%s did not invalidate stamp", name)
			}
			writeRepoFile(t, dir, name, string(b))
		})
	}
	other, err := ComputeStampKey(dir, []string{"pkg"}, "test")
	if err != nil || other == before {
		t.Fatalf("gate set not hashed: %s %v", other, err)
	}
	if stampKey(t, dir, "pkg", "dep") != stampKey(t, dir, "./dep", "./pkg", "pkg") {
		t.Fatal("package order or duplicates changed the key")
	}
}

func TestStampFreshnessAndPrune(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	key := strings.Repeat("a", 64)
	meta := StampMeta{Passed: true, Gates: DefaultStampGates}
	if err := WriteStamp(key, meta); err != nil {
		t.Fatal(err)
	}
	if _, ok := HasFreshStamp(key, 0); !ok {
		t.Fatal("new green stamp not found")
	}
	meta.Created = time.Now().Add(-time.Hour)
	if err := WriteStamp(key, meta); err != nil {
		t.Fatal(err)
	}
	if _, ok := HasFreshStamp(key, 0); ok {
		t.Fatal("expired stamp accepted")
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stampPath(key), old, old); err != nil {
		t.Fatal(err)
	}
	if err := WriteStamp(strings.Repeat("b", 64), StampMeta{Passed: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stampPath(key)); !os.IsNotExist(err) {
		t.Fatalf("old stamp was not pruned: %v", err)
	}
	if err := WriteStamp("../bad", meta); err == nil {
		t.Fatal("unsafe stamp key accepted")
	}
}

func queueStampTask(t *testing.T, task PendingTask) {
	t.Helper()
	if err := QueuePending("session", task); err != nil {
		t.Fatal(err)
	}
}

func pendingStampTasks(t *testing.T, count int) PendingVerify {
	t.Helper()
	q, err := ReadPending("session")
	if err != nil || len(q.Tasks) != count {
		t.Fatalf("pending: %+v %v; want %d tasks", q, err, count)
	}
	return q
}

func clearStampTasks(t *testing.T, q PendingVerify) {
	t.Helper()
	if err := ClearPending("session", q); err != nil {
		t.Fatal(err)
	}
}

func TestStampPendingDedupAndConcurrentArrival(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	task := PendingTask{Dir: t.TempDir(), Task: "1.5", Files: []string{"a.go", "a.go"}}
	queueStampTask(t, task)
	queueStampTask(t, task)
	q := pendingStampTasks(t, 1)
	if !slices.Equal(q.Tasks[0].Files, []string{"a.go"}) {
		t.Fatalf("duplicate paths: %+v", q)
	}
	queueStampTask(t, task)
	clearStampTasks(t, q)
	clearStampTasks(t, pendingStampTasks(t, 1))
	if _, err := os.Stat(filepath.Join(paths.StateDir(), "verify", "pending", "session.json")); !os.IsNotExist(err) {
		t.Fatalf("queue not cleared: %v", err)
	}
	if err := QueuePending("../bad", task); err == nil {
		t.Fatal("unsafe session accepted")
	}
}

func TestStampPendingCheckoutPartitions(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	one := PendingTask{Dir: t.TempDir(), Task: "same", Files: []string{"p.go"}}
	two := PendingTask{Dir: t.TempDir(), Task: "same", Files: []string{"p.go"}}
	queueStampTask(t, one)
	queueStampTask(t, two)
	q := pendingStampTasks(t, 2)
	q.Tasks = q.Tasks[:1]
	queueStampTask(t, two)
	clearStampTasks(t, q)
	left := pendingStampTasks(t, 1)
	if !paths.SamePath(left.Tasks[0].Dir, two.Dir) {
		t.Fatalf("cleared another checkout: %+v", left)
	}
}

func TestStampParentWorkspaceDeclined(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkout")
	writeRepoFile(t, dir, "go.mod", "module x\n\ngo 1.22\n")
	writeRepoFile(t, dir, "p.go", "package p\n")
	writeRepoFile(t, filepath.Dir(dir), "go.work", "go 1.22\n\nuse ./checkout\n")
	t.Setenv("GOWORK", "")
	if _, err := ComputeStampKey(dir, []string{"."}, GoStampGates); err == nil {
		t.Fatal("parent workspace was stamped")
	}
	t.Setenv("GOWORK", "off")
	if _, err := ComputeStampKey(dir, []string{"."}, GoStampGates); err != nil {
		t.Fatal(err)
	}
}

func TestStampEffectiveEnvironment(t *testing.T) {
	config := filepath.Join(t.TempDir(), "goenv")
	writeRepoFile(t, filepath.Dir(config), "goenv", "GOFLAGS=-tags=from_goenv\nCGO_CFLAGS=-DFROM_GOENV\n")
	t.Setenv("GOENV", config)
	t.Setenv("GOFLAGS", "")
	t.Setenv("CGO_CFLAGS", "")
	env, err := effectiveStampEnv(stampRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	if env["GOFLAGS"] != "-tags=from_goenv" || env["CGO_CFLAGS"] != "-DFROM_GOENV" || env["GOVERSION"] == "" {
		t.Fatalf("not effective go env: %+v", env)
	}
	for _, name := range stampEnvNames {
		t.Run(name, func(t *testing.T) {
			before := stampDigest(nil, nil, GoStampGates, env)
			old := env[name]
			env[name] += "changed"
			if before == stampDigest(nil, nil, GoStampGates, env) {
				t.Fatalf("%s not hashed", name)
			}
			env[name] = old
		})
	}
}

func TestStampDiffCoverageAndBase(t *testing.T) {
	dir := stampRepo(t)
	key := func(base string, files ...string) string {
		t.Helper()
		k, err := ComputeDiffStampKey(context.Background(), dir, base, []string{"pkg"}, files)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	partial := key("HEAD", "pkg/p.go")
	if partial == key("HEAD", "pkg/p.go", "pkg/p_test.go") {
		t.Fatal("partial file proof covered unchecked file")
	}
	repoGit(t, dir, "add", "-A")
	repoGit(t, dir, "commit", "-qm", "new base")
	if partial == key("HEAD", "pkg/p.go") {
		t.Fatal("comparison tree not hashed")
	}
}

func TestStampEnvironmentMemoAndToolchain(t *testing.T) {
	dir := stampRepo(t)
	bin := t.TempDir()
	marker := filepath.Join(bin, "calls")
	body := "#!/bin/sh\nprintf x >> '" + marker + "'\nprintf '%s' '{\"GOVERSION\":\"go1.test\",\"GOOS\":\"test\"}'\n"
	writeRepoFile(t, bin, "go", body)
	if err := os.Chmod(filepath.Join(bin, "go"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	first := stampKey(t, dir, "pkg")
	if second := stampKey(t, dir, "pkg"); second != first {
		t.Fatal("stable environment changed key")
	}
	calls, err := os.ReadFile(marker)
	if err != nil || string(calls) != "x" {
		t.Fatalf("go env not memoized: %q %v", calls, err)
	}
	writeRepoFile(t, bin, "go", strings.ReplaceAll(body, "go1.test", "go2.test-new"))
	if first == stampKey(t, dir, "pkg") {
		t.Fatal("actual Go executable change did not invalidate stamp")
	}
}

func TestStampWorkspaceArrivalInvalidatesMemo(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "checkout")
	writeRepoFile(t, dir, "go.mod", "module x\n\ngo 1.22\n")
	writeRepoFile(t, dir, "p.go", "package p\n")
	t.Setenv("GOWORK", "")
	stampKey(t, dir, ".")
	writeRepoFile(t, root, "go.work", "go 1.22\n\nuse ./checkout\n")
	if _, err := ComputeStampKey(dir, []string{"."}, GoStampGates); err == nil {
		t.Fatal("memo hid a newly discovered parent workspace")
	}
}

func TestStampGOENVChangeInvalidatesMemo(t *testing.T) {
	dir, configDir := stampRepo(t), t.TempDir()
	writeRepoFile(t, configDir, "goenv", "CGO_CFLAGS=-DFIRST\n")
	t.Setenv("GOENV", filepath.Join(configDir, "goenv"))
	t.Setenv("CGO_CFLAGS", "")
	first := stampKey(t, dir, "pkg")
	writeRepoFile(t, configDir, "goenv", "CGO_CFLAGS=-DSECOND\n")
	if first == stampKey(t, dir, "pkg") {
		t.Fatal("memo hid changed GOENV settings")
	}
}

func TestStampDiffProofIncludesWholeBuildScope(t *testing.T) {
	dir := stampRepo(t)
	files := []string{"pkg/p.go"}
	key, err := ComputeDiffStampKey(context.Background(), dir, "HEAD", []string{"pkg"}, files)
	if err != nil {
		t.Fatal(err)
	}
	goKey := goProofKey(t, dir, "pkg")
	writeRepoFile(t, dir, "unrelated/p.go", "package unrelated\nconst N = 1\n")
	next, err := ComputeDiffStampKey(context.Background(), dir, "HEAD", []string{"pkg"}, files)
	if err != nil || key == next {
		t.Fatalf("whole-build scope missing from diff proof: %s %v", next, err)
	}
	if goKey != goProofKey(t, dir, "pkg") {
		t.Fatal("narrow Go test proof changed for unrelated package")
	}
}
