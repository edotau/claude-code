package inspect

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
)

func TestStampDiffBatchMergesAndClears(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := newDiffRepo(t)
	fake := &fakeGates{}
	fake.install(t)
	if err := QueuePending("session", PendingTask{Dir: dir, Task: "1", Files: []string{"pkg/p.go"}}); err != nil {
		t.Fatal(err)
	}
	writeRepoFile(t, dir, "extra/q.go", "package extra\n")
	o := DiffOptions{Dir: dir, Check: true, Batch: true, SessionID: "session", Paths: []string{"extra/q.go"}, JSON: true}
	code, out, errOut := runDiff(t, o)
	if code != 0 {
		t.Fatalf("batch exit %d: %s %s", code, out, errOut)
	}
	var rep DiffReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || len(rep.Files) != 2 {
		t.Fatalf("committed queue file missing: %+v %v", rep.Files, err)
	}
	if _, ok := HasFreshStamp(goProofKey(t, dir, "pkg", "extra"), 0); !ok {
		t.Fatal("batch did not stamp the union")
	}
	if _, ok := HasFreshStamp(goProofKey(t, dir, "pkg"), 0); !ok {
		t.Fatal("batch did not stamp the individually checked package")
	}
	if _, err := os.Stat(filepath.Join(paths.StateDir(), "verify", "pending", "session.json")); !os.IsNotExist(err) {
		t.Fatalf("queue retained after pass: %v", err)
	}
	if n := slices.Index(fake.calls, "go build"); n < 0 || slices.Index(fake.calls[n+1:], "go build") >= 0 {
		t.Fatalf("expected one build: %v", fake.calls)
	}
}

func TestStampDiffBatchFailuresRetainQueue(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(strconv.FormatBool(partial), func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			dir := newDiffRepo(t)
			fake := &fakeGates{fail: map[string]string{"go build": "build failed"}}
			fake.install(t)
			if err := QueuePending("s", PendingTask{Dir: dir, Task: "1", Files: []string{"pkg/p.go"}}); err != nil {
				t.Fatal(err)
			}
			o := DiffOptions{Dir: dir, Check: true, Batch: true, SessionID: "s"}
			if partial {
				o.Gates = []string{"test"}
			}
			code, out, _ := runDiff(t, o)
			if !partial && code != DiffExitGate {
				t.Fatalf("failed check exit %d: %s", code, out)
			}
			q, err := ReadPending("s")
			if err != nil || len(q.Tasks) != 1 {
				t.Fatalf("queue lost: %+v %v", q, err)
			}
			if _, ok := HasFreshStamp(goProofKey(t, dir, "pkg"), 0); ok {
				t.Fatal("failed or partial gates wrote a default pass stamp")
			}
		})
	}
}

func TestStampDiffRejectsMutationDuringCheck(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := newDiffRepo(t)
	(&fakeGates{}).install(t)
	orig := gateRun
	gateRun = func(ctx context.Context, root, name string, args ...string) (string, error) {
		if name == "go" && args[0] == "build" {
			writeRepoFile(t, dir, "pkg/p.go", "package pkg\n// changed during the gates\n")
		}
		return orig(ctx, root, name, args...)
	}
	gi := newGateInput(dir, "HEAD", []DiffFile{{Path: "pkg/p.go", Status: "M"}})
	res := checkedGates(context.Background(), gi, nil)
	if !slices.ContainsFunc(res, func(g GateResult) bool { return g.Name == "stamp" && g.Status == "FAIL" }) {
		t.Fatalf("mutation was stamped: %+v", res)
	}
	if _, ok := HasFreshStamp(goProofKey(t, dir, "pkg"), 0); ok {
		t.Fatal("unvalidated contents got a stamp")
	}
}

func writeRepoFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repoGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)
	if _, err := proc.RunGit(context.Background(), dir, full...); err != nil {
		t.Fatal(err)
	}
}

// newDiffRepo commits pkg/p.go, pkg/p_test.go and other/o.txt in a fresh repo.
func newDiffRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repoGit(t, dir, "init", "-q")
	writeRepoFile(t, dir, "go.mod", "module x\n\ngo 1.22\n")
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\n\nfunc P() int { return 1 }\n")
	writeRepoFile(t, dir, "pkg/p_test.go", "package pkg\n")
	writeRepoFile(t, dir, "other/o.txt", "one\n")
	repoGit(t, dir, "add", "-A")
	repoGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func runDiff(t *testing.T, o DiffOptions) (code int, out, errOut string) {
	t.Helper()
	var so, se bytes.Buffer
	o.Out, o.Err = &so, &se
	return Diff(o), so.String(), se.String()
}

func TestDiffGateRowsKeepOnlyFailuresAndSkips(t *testing.T) {
	var out bytes.Buffer
	renderDiff(DiffOptions{Out: &out}, DiffReport{Gates: []GateResult{
		{Name: "gofmt", Status: "ok", Seconds: 0.1},
		{Name: "build", Status: "FAIL", Seconds: 0.2, Findings: []Finding{{Tool: "go build", Message: "build failed"}}},
		{Name: "vet", Status: "skip", Reason: "build failed"},
	}}, false)
	got := out.String()
	wantInOutput(t, got, "FAIL build (0.2s)", "go build: build failed", "skip vet (build failed)", "gates 1 ok, 1 FAIL, 1 skip")
	if strings.Contains(got, "ok gofmt") {
		t.Errorf("passing gate row repeated the summary: %s", got)
	}
}

// squeeze collapses runs of spaces so assertions do not depend on column padding.
func squeeze(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.Join(lines, "\n")
}

func TestDiffListsChangesAndOutsidePath(t *testing.T) {
	dir := newDiffRepo(t)
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\n\nfunc P() int { return 2 }\n\nfunc Q() int { return 3 }\n")
	writeRepoFile(t, dir, "pkg/new_test.go", "package pkg\n\n// t\n")
	writeRepoFile(t, dir, "other/extra.md", "x\ny\n")
	repoGit(t, dir, "mv", "other/o.txt", "other/o r.txt")

	code, out, errOut := runDiff(t, DiffOptions{Dir: dir, Paths: []string{"pkg"}})
	if code != DiffExitOutside || errOut != "" {
		t.Fatalf("exit %d stderr %q, want 3 and none\n%s", code, errOut, out)
	}
	for _, want := range []string{
		"? +2 -0 other/extra.md\n",
		"R +0 -0 other/o.txt -> other/o r.txt\n",
		"? +3 -0 pkg/new_test.go (test)\n",
		"M +3 -1 pkg/p.go\n",
		"outside --path (2):\nother/extra.md\nother/o r.txt\n",
		"agents diff: 4 files +8 -1 (prod +5 -1, test +3 -0); 2 outside --path\n",
	} {
		if !strings.Contains(squeeze(out), want) {
			t.Errorf("text output lacks %q:\n%s", want, out)
		}
	}

	code, jsonOut, _ := runDiff(t, DiffOptions{Dir: dir, Paths: []string{"pkg"}, JSON: true})
	var rep DiffReport
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil || code != DiffExitOutside {
		t.Fatalf("json exit %d err %v\n%s", code, err, jsonOut)
	}
	checkJSONMatchesText(t, rep)
}

func checkJSONMatchesText(t *testing.T, rep DiffReport) {
	t.Helper()
	var paths []string
	for _, f := range rep.Files {
		paths = append(paths, f.Path)
	}
	if !slices.Equal(paths, []string{"other/extra.md", "other/o r.txt", "pkg/new_test.go", "pkg/p.go"}) {
		t.Errorf("json paths = %v", paths)
	}
	if !slices.Equal(rep.Outside, []string{"other/extra.md", "other/o r.txt"}) || rep.Files[1].From != "other/o.txt" {
		t.Errorf("json outside = %v, rename from %q", rep.Outside, rep.Files[1].From)
	}
	if tot := rep.Totals; tot.Added != 8 || tot.Deleted != 1 || tot.Test.Added != 3 || tot.Prod.Added != 5 {
		t.Errorf("json totals = %+v", tot)
	}
}

func TestDiffBaseAndCleanTree(t *testing.T) {
	dir := newDiffRepo(t)
	if code, out, _ := runDiff(t, DiffOptions{Dir: dir}); code != 0 || out != "agents diff: no changes\n" {
		t.Errorf("clean tree: exit %d out %q", code, out)
	}
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\n")
	repoGit(t, dir, "commit", "-q", "-am", "second")
	code, out, _ := runDiff(t, DiffOptions{Dir: dir, Base: "HEAD~1"})
	if code != 0 || !strings.Contains(squeeze(out), "M +0 -2 pkg/p.go\n") {
		t.Errorf("--base HEAD~1: exit %d\n%s", code, out)
	}
	if code, _, errOut := runDiff(t, DiffOptions{Dir: dir, Base: "--output=x"}); code != DiffExitUsage || !strings.Contains(errOut, "must not start") {
		t.Errorf("flag-shaped base: exit %d stderr %q", code, errOut)
	}
}

func TestDiffRejectsBadPath(t *testing.T) {
	dir := newDiffRepo(t)
	for path, msg := range map[string]string{"../x": "inside the repo root", "pkg/../../x": "inside the repo root",
		"/etc": "inside the repo root", t.TempDir(): "inside the repo root", "a,b": "repeat --path"} {
		code, out, errOut := runDiff(t, DiffOptions{Dir: dir, Paths: []string{path}})
		if code != DiffExitUsage || out != "" || !strings.Contains(errOut, msg) {
			t.Errorf("--path %q: exit %d stdout %q stderr %q, want 2 and %q", path, code, out, errOut, msg)
		}
	}
	real, err := filepath.EvalSymlinks(dir) // macOS: /var/folders/... resolves to /private/var/folders/...
	if err != nil {
		t.Fatal(err)
	}
	got, err := cleanPaths(dir, []string{filepath.Join(dir, "pkg"), filepath.Join(real, "pkg"), "./pkg/p.go", "rules/**/*.md"})
	if err != nil || !slices.Equal(got, []string{"pkg", "pkg", "pkg/p.go", "rules/**/*.md"}) {
		t.Errorf("cleanPaths = %v, %v", got, err)
	}
}

func TestInPathsPrefixAndGlob(t *testing.T) {
	sel := []string{"internal/memory", "rules/**/*.md", "internal/cli/memory.go"}
	for p, want := range map[string]bool{
		"internal/memory/a.go": true, "internal/memory2/a.go": false, "rules/a.md": true, "rules/x/y/a.md": true,
		"rules/a.txt": false, "internal/cli/memory.go": true, "internal/cli/memory_test.go": false,
	} {
		if got := inPaths(sel, p); got != want {
			t.Errorf("inPaths(%q) = %v, want %v", p, got, want)
		}
	}
}

type dirtyEngine struct{ act func() }

func (*dirtyEngine) Name() string     { return "fake" }
func (*dirtyEngine) Caps() agent.Caps { return agent.Caps{} }
func (*dirtyEngine) Available() error { return nil }
func (e *dirtyEngine) Run(context.Context, agent.RunOptions, wire.Sink) (wire.Result, error) {
	e.act()
	return wire.Result{Answer: "done"}, nil
}

func dirtyDispatch(t *testing.T, dir string) string {
	t.Helper()
	write := func(p, body string, stamp int64) {
		t.Helper()
		writeRepoFile(t, dir, p, body)
		ts := time.Unix(stamp, 0)
		if err := os.Chtimes(filepath.Join(dir, p), ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"edited.txt", "idle.txt", "owned.txt", "reverted.txt"} {
		write(p, "base\n", 1)
	}
	repoGit(t, dir, "add", "edited.txt", "idle.txt", "owned.txt", "reverted.txt")
	repoGit(t, dir, "commit", "-qm", "fixture")
	for _, p := range []string{"edited.txt", "idle.txt", "owned.txt", "reverted.txt"} {
		write(p, "dirt\n", 2)
	}
	engine := &dirtyEngine{act: func() {
		write("edited.txt", "edit\n", 3)
		write("owned.txt", "edit\n", 3)
		write("reverted.txt", "base\n", 3)
	}}
	req := agent.RunOptions{Agent: "fake", Prompt: "fixture", WorkDir: dir}
	p := &agent.Prepared{Opts: req, Record: agent.ClaimRecord("fake", req, time.Now()), Chain: agent.Single(engine)}
	if _, err := (&agent.Client{}).Exec(context.Background(), p, wire.Discard, agent.RunRecordedOpts{}); err != nil {
		t.Fatal(err)
	}
	return p.Record.ID
}

func TestDiffFlagsPreDirtyOutside(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(proc.DispatchIDEnv, "")
	dir := newDiffRepo(t)
	id := dirtyDispatch(t, dir)
	for _, verify := range []string{id, ""} {
		t.Setenv(proc.DispatchIDEnv, id)
		code, out, errOut := runDiff(t, DiffOptions{Dir: dir, Verify: verify, Paths: []string{"owned.txt"}})
		if code != DiffExitOutside || errOut != "" {
			t.Fatalf("exit %d stderr %q\n%s", code, errOut, out)
		}
		for _, p := range []string{"edited.txt", "reverted.txt"} {
			wantInOutput(t, out, p+": was already modified before dispatch "+id+" started and is outside --path")
		}
		if strings.Count(out, ": was already modified before dispatch ") != 2 {
			t.Fatalf("unexpected warnings:\n%s", out)
		}
	}
	repoGit(t, dir, "add", "edited.txt", "idle.txt", "owned.txt")
	repoGit(t, dir, "commit", "-qm", "keep other edits")
	code, out, _ := runDiff(t, DiffOptions{Dir: dir, Verify: id, Paths: []string{"edited.txt", "idle.txt", "owned.txt"}})
	if code != DiffExitOutside {
		t.Fatalf("reverted file alone: exit %d\n%s", code, out)
	}
	wantInOutput(t, out, "outside --path (1):\n  reverted.txt: was already modified")
}

func goProofKey(t *testing.T, dir string, pkgs ...string) string {
	t.Helper()
	key, err := ComputeStampKey(dir, pkgs, GoStampGates)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestStampOutsideNeverWritesProof(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := newDiffRepo(t)
	(&fakeGates{}).install(t)
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\nfunc P() int { return 2 }\n")
	writeRepoFile(t, dir, "pkg/a.go", "package pkg\n")
	code, out, errOut := runDiff(t, DiffOptions{Dir: dir, Check: true, Paths: []string{"pkg/p.go"}})
	if code != DiffExitOutside {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if _, ok := HasFreshStamp(goProofKey(t, dir, "pkg"), 0); ok {
		t.Fatal("outside-scope check wrote a pass stamp")
	}
}

func TestStampGoInputOwners(t *testing.T) {
	dir := newDiffRepo(t)
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\nimport _ \"embed\"\n//go:embed prompts/*\nvar P string\n")
	writeRepoFile(t, dir, "pkg/testdata/bad.go", "not valid Go")
	for _, name := range []string{"pkg/testdata/bad.go", "pkg/testdata/sub/input.txt", "pkg/prompt.md", "pkg/prompts/p.md"} {
		t.Run(name, func(t *testing.T) {
			files := []DiffFile{{Path: name, Status: "M"}}
			gi := newGateInput(dir, "HEAD", files)
			includeGoInputs(&gi, files)
			if !slices.Equal(gi.pkgs, []string{"./pkg"}) {
				t.Fatalf("%s owners = %v", name, gi.pkgs)
			}
		})
	}
	if owner := goInputOwner(dir, "docs/guide.md"); owner != "" {
		t.Fatalf("standalone docs owner = %s", owner)
	}
}

func TestStampBatchPartitionAndCustomFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir, other := newDiffRepo(t), newDiffRepo(t)
	(&fakeGates{}).install(t)
	queueStampTask(t, PendingTask{Dir: other, Task: "other", Files: []string{"pkg/p.go"}})
	queueStampTask(t, PendingTask{Dir: dir, Task: "local", Files: []string{"pkg/p.go"},
		Checks: []PendingCheck{{Command: "go test -race ./pkg"}}})
	old := pendingRun
	t.Cleanup(func() { pendingRun = old })
	fail, calls := true, 0
	pendingRun = func(_ context.Context, root string, check PendingCheck) (string, error) {
		calls++
		if !paths.SamePath(root, dir) || check.Command != "go test -race ./pkg" {
			t.Fatalf("wrong custom check: %s %+v", root, check)
		}
		if fail {
			return "custom failed", os.ErrInvalid
		}
		return "", nil
	}
	o := DiffOptions{Dir: dir, Check: true, Batch: true, SessionID: "session"}
	if code, out, _ := runDiff(t, o); code != DiffExitGate {
		t.Fatalf("custom failure ignored: %d %s", code, out)
	}
	pendingStampTasks(t, 2)
	if _, ok := HasFreshStamp(goProofKey(t, dir, "pkg"), 0); ok {
		t.Fatal("custom failure wrote pass stamp")
	}
	fail = false
	if code, out, _ := runDiff(t, o); code != 0 {
		t.Fatalf("batch failed: %d %s", code, out)
	}
	left := pendingStampTasks(t, 1)
	if calls != 2 || !paths.SamePath(left.Tasks[0].Dir, other) {
		t.Fatalf("custom calls %d; remaining %+v", calls, left)
	}
}

func TestStampQueuedCommandsExecuteAndRejectShell(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	marker := filepath.Join(dir, "called")
	writeRepoFile(t, bin, "go", "#!/bin/sh\nprintf '%s\\n' \"$*\" > '"+marker+"'\n")
	if err := os.Chmod(filepath.Join(bin, "go"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	check := PendingCheck{Command: "go test -race ./pkg"}
	if out, err := runPendingCheck(context.Background(), dir, check); err != nil {
		t.Fatalf("custom check failed: %s %v", out, err)
	}
	b, err := os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(b)) != "test -race ./pkg" {
		t.Fatalf("custom args: %q %v", b, err)
	}
	check.Command = "go test ./pkg; touch injected"
	if _, err := runPendingCheck(context.Background(), dir, check); err == nil {
		t.Fatal("untrusted shell syntax accepted")
	}
}

func TestStampGeneratedToolchainFailureRetainsQueue(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := newDiffRepo(t)
	(&fakeGates{}).install(t)
	queueStampTask(t, PendingTask{Dir: dir, Task: "python", Files: []string{"pkg/p.go"},
		Checks: []PendingCheck{{Command: "exit 7", Generated: true}}})
	code, out, _ := runDiff(t, DiffOptions{Dir: dir, Check: true, Batch: true, SessionID: "session"})
	if code != DiffExitGate {
		t.Fatalf("toolchain failure lost: %d %s", code, out)
	}
	pendingStampTasks(t, 1)
}
