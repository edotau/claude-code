package safety

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/platform/proc"
)

// suiteRuns are whole-suite test runs in every language and wrapper the gate reads (point 2 of the plan).
var suiteRuns = []string{
	"go test ./...", "go test ./internal/...", "go test ...", "go test all", "go test $(go list ./...)",
	"go test `go list ./...`", "go test -count=1 -race ./...", "go test -v -timeout 5m ./...",
	"rtk go test ./...", "rtk bypass go test ./...", "FOO=1 go test ./...", "env FOO=1 go test ./...",
	"time go test ./...", "ls && go test ./...", "go build ./... ; go test ./...", "go test ./... 2>&1 | tail -5",
	"go test ./... # the suite", "bash -c 'go test ./...'", `sh -c "cd x && go test ./..."`, "eval 'go test ./...'",
	`bash -c "sh -c 'eval go test ./...'"`, "eval eval eval eval go test ./...", "go test ./... -args -test.v",
	"make test", "make audit", "make verify", "rtk make test", "make -j4 test", "make test build",
	"pytest", "pytest src/", "pytest src/*.py", "pytest -q -x tests", "pytest -n 4", "pytest . --ignore=tests/slow", "pytest --junitxml report.xml", "pytest tests.v2/", "python -m pytest",
	"python3 -m pytest tests", "python3 -u -m pytest", "tox", "tox -- tests/",
	"npm test", "npm run test", "npm run test:unit", "pnpm test", "pnpm -r test", "yarn test", "bun test",
	"npm run vitest ./src", "npm run jest", "npm test src", "npx vitest", "npx vitest run src", "npx jest", "vitest run",
	"jest", "npx vitest run --reporter dot",
	"cargo test", "cargo test --workspace", "cargo test -- --nocapture",
	"mvn test", "mvn clean test", "mvn test -DskipTests=false", "./mvnw test", "gradle test", "./gradlew test", "./gradlew clean test",
	"claude-code worktree gate --run", "claude-code worktree gate --run --scope=''",
	"claude-code worktree gate --run --scope=' , '", "claude-code worktree gate -run", "claude-code worktree gate --run=TRUE", "claude-code worktree land --full-verify",
}

// narrowRuns never match: narrowed or non-test runs, and runner words that are only text (point 3 of the plan).
var narrowRuns = []string{
	"go test -run '^TestX$' ./internal/pkg", "go test -run '^TestX$' ./...", "go test -run=TestX ./...",
	"go test -bench . ./...", "go test -list . ./...", "go test -fuzz FuzzX ./internal/pkg", "go test -c ./...",
	"go test ./... -args -test.run=TestX", "go test ./internal/pkg", "go test", "go vet ./...", "go build ./...",
	"go test -coverpkg ./... ./internal/x", "go test -n ./...", "go test -run . ./...", "go test ./... -run", "go test $(printf ./internal/x)",
	"pytest tests/test_x.py::test_y", "pytest -k name", "pytest -kname src", "pytest tests/test_x.py",
	"python -m pytest -k foo src", "pytest --collect-only", "pytest README.md",
	"pytest 'tests/test_x.py[param]'", "python -X",
	"pytest -mfast", `pytest "$TEST_TARGET"`, "pytest --setup-plan", "python -h -m pytest", "tox -e lint", "tox -- tests/test_x.py::test_y",
	"jest -t name", "npx jest -t name", "vitest run src/x.test.ts", "npx vitest run src/x.test.ts",
	"npm test -- -t foo", "npm run vitest src/x.test.ts", "pnpm test src/a.test.ts", "npx jest src/x", "npx vitest 'src/**/*.test.ts'",
	"npx jest --testPathPatterns=src/x.test.ts", "npx jest 'src/x[.]test[.]tsx?$'", "npm install", "npm run build",
	"cargo test name", "cargo test -p crate", "cargo build", "mvn test -Dtest=FooTest", "mvn package", "mvn test -DskipTests", "mvn --log-file test validate", "mvn test -D test=FooTest",
	"cargo test -pmycrate", "gradle --project-dir test tasks",
	"gradle test --tests FooTest", "gradle build -x test", "gradle test -x test", "gradle test --exclude-task=test",
	"claude-code worktree gate --run --scope internal/x", "claude-code worktree land", "claude-code worktree gate",
	"claude-code worktree gate --run=false", "claude-code worktree land --full-verify=false",
	"claude-code worktree gate --run --help", "claude-code worktree gate --run --run=false",
	"make build", "make -n test", "make -ns test", "make --include-dir test build", "make lint",
	"echo go test ./...", "echo 'make test'", "grep -rn 'go test ./...' docs", "git commit -m 'run go test ./...'",
	"git log --grep 'go test ./...'", "ls # go test ./...", "# go test ./...\nls",
	"cat <<'EOF' > notes.md\ngo test ./...\npytest\nnpm test\nEOF", "cat <<pytest > f\ntext\npytest",
	"cat <<A <<B\nfirst\nA\ngo test ./...\nB",
	"git commit -m \"$(cat <<'EOF'\nrun make test and go test ./...\nEOF\n)\"",
	"", "echo 'unterminated go test ./...", "ls -la",
}

// suiteCheckouts makes a main checkout and a linked worktree of it, shaped as git lays them on disk.
func suiteCheckouts(t *testing.T) (mainDir, linked string) {
	t.Helper()
	root := t.TempDir()
	mainDir, linked = filepath.Join(root, "repo"), filepath.Join(root, "wt")
	gitdir := filepath.Join(mainDir, ".git", "worktrees", "wt")
	for _, d := range []string{gitdir, linked} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writes := map[string]string{filepath.Join(mainDir, ".git", "HEAD"): "ref: refs/heads/main\n",
		filepath.Join(linked, ".git"): "gitdir: " + gitdir + "\n", filepath.Join(gitdir, "commondir"): "../..\n"}
	for p, body := range writes {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return mainDir, linked
}

func suitePayload(cmd, cwd, agent string) *core.Input {
	doc := map[string]any{"tool_name": "Bash", "cwd": cwd, "tool_input": map[string]any{"command": cmd}}
	if agent != "" {
		doc["agent_id"] = agent
	}
	payload, _ := json.Marshal(doc)
	return core.ParseInput(bytes.NewReader(payload))
}

// The suite is denied to any agent and in any linked worktree; the main session in the main checkout runs it.
func TestSuiteVerdict(t *testing.T) {
	t.Setenv(proc.DispatchIDEnv, "")
	mainDir, linked := suiteCheckouts(t)
	callers := []struct {
		name, cwd, agent string
		denied           bool
	}{{"main session, main checkout", mainDir, "", false}, {"main session, linked worktree", linked, "", true},
		{"subagent, main checkout", mainDir, "a1", true}, {"subagent, linked worktree", linked, "a1", true}}
	for _, c := range callers {
		for _, cmd := range suiteRuns {
			r := suiteVerdict(suitePayload(cmd, c.cwd, c.agent))
			if c.denied != (r.Decision == core.Block) || c.denied && !strings.Contains(r.Reason, "main session") {
				t.Errorf("%s: %q: got %v %q, want denied=%v", c.name, cmd, r.Decision, r.Reason, c.denied)
			}
		}
		for _, cmd := range narrowRuns {
			if r := suiteVerdict(suitePayload(cmd, c.cwd, c.agent)); r.Decision != core.Allow {
				t.Errorf("%s: %q: got %v %q, want allow", c.name, cmd, r.Decision, r.Reason)
			}
		}
	}
	suiteMainSessionMoves(t, mainDir, linked)
	if r := suiteVerdict(suitePayload("go test ./...", mainDir, "")); r.Reason != "" {
		t.Errorf("allowed run carries a reason %q", r.Reason)
	}
	t.Setenv(proc.DispatchIDEnv, "d1")
	r := Scan(suitePayload("go test ./...", mainDir, ""))
	if r.Decision != core.Block || !strings.Contains(r.Reason, goForm) || strings.Count(r.Reason, "\n") > 1 {
		t.Errorf("Scan, dispatched agent: got %v %q, want a two-line block naming %q", r.Decision, r.Reason, goForm)
	}
}

// suiteMainSessionMoves: the main session is judged in the directory each command runs in, unknown ones allow.
func suiteMainSessionMoves(t *testing.T, mainDir, linked string) {
	t.Helper()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(linked, alias); err != nil {
		t.Fatal(err)
	}
	long := "cat > f <<'EOF'\n" + strings.Repeat("x\n", 9<<10) + "EOF\ncd " + linked + " ; go test ./..."
	for _, c := range []struct {
		cwd, cmd string
		deny     bool
	}{
		{mainDir, "cd " + linked + " && go test ./...", true}, {mainDir, "env -C " + linked + " go test ./...", true},
		{mainDir, "rtk bypass env -C " + linked + " go test ./...", true}, {mainDir, "make -C " + linked + " test", true},
		{mainDir, "make --directory " + linked + " test", true}, {mainDir, long, true}, {alias, "go test ./...", true},
		{mainDir, "claude-code worktree gate --run --dir " + linked, true},
		{linked, "claude-code worktree gate --run --dir " + mainDir, false},
		{linked, "make -C " + linked + " -C " + mainDir + " test", false},
		{linked, `cd "$DEST" && bash -c 'go test ./...'`, false}, {linked, "env -C " + mainDir + " go test ./...", false},
	} {
		if r := suiteVerdict(suitePayload(c.cmd, c.cwd, "")); (r.Decision == core.Block) != c.deny {
			t.Errorf("main session in %s: %.70q: got %v, want denied=%v", filepath.Base(c.cwd), c.cmd, r.Decision, c.deny)
		}
	}
}

// BenchmarkSuiteGate is the rule's own cost (the early exit, a narrowed run, a denied run with its audit line).
func BenchmarkSuiteGate(b *testing.B) {
	benchSettings(b)
	b.Setenv(proc.DispatchIDEnv, "")
	for _, c := range []struct{ name, cmd, agent string }{
		{"non-test", "git status", "a1"}, {"narrow-go-test", "go test -run '^TestX$' ./internal/x", "a1"},
		{"denied-suite", "go test ./...", "a1"},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			in := suitePayload(c.cmd, b.TempDir(), c.agent)
			benchSink = suiteVerdict(in)
			for b.Loop() {
				benchSink = suiteVerdict(in)
			}
		})
	}
}
