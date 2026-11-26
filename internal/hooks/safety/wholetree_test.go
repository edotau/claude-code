package safety

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/proc"
)

// The exact suite-run shapes: denied to a leaf, allowed to the main session.
var wholeTreeDeny = []string{
	"go test ./...", "go test ./internal/worktree/...", "go test ./internal/...", "go test -count=1 ./...", "go test -v -race ./...", "go test ...", "go test all",
	"rtk go test ./...", "rtk bypass go test ./...", "cd /x && go test ./...", "FOO=1 go test ./...",
	"ls && go test ./...", "go build ./... && go test ./...", "go test ./... > out.txt", "go test ./... 2>&1 | tail",
	"time go test ./...", "env X=1 go test ./...", "pytest", "pytest .", "pytest src", "pytest tests", "pytest -q -x tests",
	"pytest -v -s", "python -m pytest", "python3 -m pytest tests", "pytest > out", "echo hi | pytest",
	"npm test", "npm run test", "pnpm test", "yarn test", "bun test", "npm test src", "npx vitest run src", "npx jest",
	"vitest", "jest", "vitest run", "npm run test --run", "make test", "make audit", "make verify", "rtk make test",
	"bash -c 'go test ./...'", "sh -c \"cd x && make test\"", "zsh -c 'pytest'",
}

// Everything that is not exactly a suite-run shape passes, the reviews' false denies included.
var wholeTreeAllow = []string{
	"go test -run '^TestX$' ./internal/x", "go test -run=TestX ./...", "go test -run . ./...", "go test -run '' ./...",
	"go test ./internal/x", "go test", "go vet ./...",
	"go build ./...", "go test -list . ./...", "go test -c ./...", "go test -C dir ./...", "go test -bench . ./...",
	"go test ./pkg -args ./internal/...", "go test -fuzz F ./...", "go test -coverpkg ./... ./internal/x",
	"go test -tags all ./internal/x", "pytest tests/test_x.py", "pytest tests/test_x.py::test_y", "pytest -k foo",
	"pytest -kfoo", "pytest tests/unit", "pytest tests/test_a.py > out", "pytest --version", "pytest --collect-only",
	"pytest --help", "pytest -c cfg tests/a.py", "pytest --rootdir d tests/a.py", "pytest -v -s tests/unit/test_a.py",
	"pytest -m slow tests/unit", "pytest --cov pkg", "pytest -n 4", "python -m pytest -k foo src",
	"python3 -u -m pytest", "python3 script.py", "npm test -- -t foo", "npm test -- -tfoo", "npx vitest run src/a.test.ts",
	"vitest -t foo src", "jest a.test.js", "jest -i a.test.js", "pnpm test src/a.test.ts", "yarn jest a.test.js",
	"bun test a.test.ts", "npx jest --listTests", "jest --help", "npm install", "make build", "make build > test",
	"make -n test", "make test build", "make -j4 test", "make lint", "ls ./...", "echo go test ./...",
	"cd /x && go vet ./...", "echo 'go test ./...'", "grep -n 'make test' README.md", "git commit -m 'run go test ./...'",
	"cat <<'EOF' > doc.md\ngo test ./...\nEOF", "git commit -m \"$(cat <<'EOF'\nrun make test\nEOF\n)\"",
	"python3 - <<'EOF'\nimport os\nos.system('go test ./...')\ngo test ./...\nmake test\nEOF",
	"bash -c 'go vet ./...'", "eval 'pytest'", "echo $(go test ./...)", "bash -c \"bash -c 'make test'\"", "",
	"echo 'unterminated", "echo \"unterminated", strings.Repeat("a b ", 1<<18),
	"go test ./... -args -test.run=TestX", "go test ./... --run TestX", "go test --run=X ./...", "go test -test.run=X ./...",
	strings.Repeat("echo|", 25000) + "go test ./...", strings.Repeat("cat <<A\n", 50000) + "go test ./...",
	strings.Repeat("$((", 80000), strings.Repeat("a<<A\n", 1999) + "go test ./...",
	"bash -c $'cat <<EOF > /tmp/notes.md\ngo test ./...\nEOF'",
}

// within fails t when fn runs longer than limit: the odd inputs must not make the hook quadratic.
func within(t *testing.T, limit time.Duration, fn func()) {
	t.Helper()
	start := time.Now()
	fn()
	if d := time.Since(start); d > limit {
		t.Errorf("took %v, limit %v", d, limit)
	}
}

// A leaf's exact suite run is blocked; the main session and every other shape pass.
func TestWholeTreeVerdict(t *testing.T) {
	t.Setenv(proc.DispatchIDEnv, "")
	run := func(cmd, agent string) core.Result {
		doc := map[string]any{"tool_name": "Bash", "cwd": t.TempDir(), "tool_input": map[string]any{"command": cmd}}
		if agent != "" {
			doc["agent_id"] = agent
		}
		payload, _ := json.Marshal(doc)
		return wholeTreeVerdict(core.ParseInput(bytes.NewReader(payload)))
	}
	for _, c := range wholeTreeDeny {
		if r := run(c, "a1"); r.Decision != core.Block || !strings.Contains(r.Reason, "go test -run") {
			t.Errorf("leaf %q: got %v %q, want block", c, r.Decision, r.Reason)
		}
		if r := run(c, ""); r.Decision != core.Allow {
			t.Errorf("main %q: got %v, want allow", c, r.Decision)
		}
	}
	within(t, 3*time.Second, func() {
		for _, c := range wholeTreeAllow {
			if r := run(c, "a1"); r.Decision != core.Allow {
				t.Errorf("leaf %.60q: got %v %q, want allow", c, r.Decision, r.Reason)
			}
		}
	})
	t.Setenv(proc.DispatchIDEnv, "d1")
	if r := run("go test ./...", ""); r.Decision != core.Block {
		t.Errorf("dispatched agent by env: got %v, want block", r.Decision)
	}
	scan := `{"tool_name":"Bash","agent_id":"a1","tool_input":{"command":"make test"}}`
	if r := scanPayload(strings.NewReader(scan)); r.Decision != core.Block {
		t.Errorf("Scan leaf make test: got %v, want block", r.Decision)
	}
}
