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

// Every test run: denied to a leaf, allowed to the main session.
var wholeTreeDeny = []string{
	"go test ./...", "go test ./internal/worktree/...", "go test ./internal/...", "go test -count=1 ./...", "go test -v -race ./...", "go test ...", "go test all",
	"rtk go test ./...", "rtk bypass go test ./...", "cd /x && go test ./...", "FOO=1 go test ./...",
	"ls && go test ./...", "go build ./... && go test ./...", "go test ./... > out.txt", "go test ./... 2>&1 | tail",
	"time go test ./...", "env X=1 go test ./...", "nice go test ./internal/x", "command go test", "pytest", "pytest .",
	"pytest src", "pytest tests", "pytest -q -x tests", "pytest -v -s", "python -m pytest", "python3 -m pytest tests",
	"pytest > out", "echo hi | pytest",
	"npm test", "npm run test", "pnpm test", "yarn test", "bun test", "npm test src", "npx vitest run src", "npx jest",
	"vitest", "jest", "vitest run", "npm run test --run", "make test", "make audit", "make verify", "rtk make test",
	"bash -c 'go test ./...'", "sh -c \"cd x && make test\"", "zsh -c 'pytest'",
	"go test -run '^TestX$' ./internal/x", "go test -run=TestX ./...", "go test -run . ./...", "go test -run '' ./...",
	"go test ./internal/x", "go test", "go test -h", "go test --help", "go test -list . ./...", "go test -c ./...",
	"go test -C dir ./...", "go test -bench . ./...", "go test ./pkg -args ./internal/...", "go test -fuzz F ./...",
	"go test -coverpkg ./... ./internal/x", "go test -tags all ./internal/x", "go test ./... -args -test.run=TestX",
	"go test ./... --run TestX", "go test --run=X ./...", "go test -test.run=X ./...", "pytest tests/test_x.py",
	"pytest tests/test_x.py::test_y", "pytest -k foo", "pytest -kfoo", "pytest tests/unit", "pytest tests/test_a.py > out",
	"pytest --version", "pytest --collect-only", "pytest --help", "pytest -c cfg tests/a.py",
	"pytest --rootdir d tests/a.py",
	"pytest -v -s tests/unit/test_a.py", "pytest -m slow tests/unit", "pytest --cov pkg", "pytest -n 4",
	"go -C dir test ./...", "npm --prefix web test", "pnpm -F app run test:unit", "yarn --cwd web t", "bun run test",
	"npm run-script test:e2e", "make -f Makefile test", "make -j4 VAR=1 audit", "make --directory sub verify",
	"python -m pytest -k foo src", "python3 -u -m pytest", "npm test -- -t foo", "npm test -- -tfoo", "npm t",
	"pnpm run test:unit", "yarn run test:all", "npx vitest run src/a.test.ts", "vitest -t foo src", "jest a.test.js",
	"jest -i a.test.js", "pnpm test src/a.test.ts", "yarn jest a.test.js", "bun test a.test.ts", "npx jest --listTests",
	"jest --help", "make -n test", "make test build", "make -j4 test", "make -C sub audit", "rtk bypass make verify",
	"make -j test", "make -j 8 test", "make --jobs test", "npx --yes vitest", "npx -y jest", "npx --no-install -q jest", "bunx --yes vitest",
	"npx -p vitest vitest", "npx --package x jest", "pnpm -w test", "pnpm -w run test:unit", "npm -w web test",
	"npm --workspace web run test", "python3 -W ignore -m pytest", "python -X dev -m pytest", "python3 -W error -u -m pytest",
	"bash -c 'go test ./...' runner", "sh -c 'make test' a b",
	"eval 'pytest'", "eval go test ./...", "echo $(go test ./...)", "x=`make test`", "bash -c \"bash -c 'make test'\"",
	"go\ttest ./...", "npm\ttest", "make\ttest", "go test ./... # don't rerun", "npm exec vitest", "pnpm exec jest",
	"pnpm dlx vitest run", "python3.11 -m pytest", "python3.12 -m pytest tests",
	"bash -lc 'go test ./...'", "sh -ec 'make test'", "X=\"$(go test ./...)\"", "echo \"out: $(pytest -q)\"",
	"yarn workspace app test", "python -m unittest", "python3 -m unittest discover", "cargo test", "cargo +nightly test --all",
}

// Text that only mentions a test word, build, vet and lint pass, and so does anything too large or odd to judge.
var wholeTreeAllow = []string{
	"go vet ./...", "go build ./...", "go list ./...", "gofmt -l .", "golangci-lint run ./...", "python3 script.py",
	"go -C dir build ./...", "go run ./cmd/x test", "go doc testing", "go tool test2json", "npm run testdata:gen",
	"npm --prefix test run build", "make -f test build", "make -C test build", "make VAR=test build",
	"npm install", "npm run build", "npm run lint", "make build", "make build > test", "make lint", "make testdata",
	"make compile", "ls ./...", "echo go test ./...", "cd /x && go vet ./...", "echo 'go test ./...'",
	"grep -n 'make test' README.md", "grep -rn 'go test' internal", "git commit -m 'run go test ./...'",
	"cat internal/x/foo_test.go", "go vet ./internal/hooks/safety/", "ls wholetree_test.go",
	"cat <<'EOF' > doc.md\ngo test ./...\nEOF", "git commit -m \"$(cat <<'EOF'\nrun make test\nEOF\n)\"",
	"python3 - <<'EOF'\nimport os\nos.system('go test ./...')\ngo test ./...\nmake test\nEOF",
	"make -j 8 build", "make --old-file test compile", "make -o test build", "make -W test build", "make --what-if test build",
	"make -I test build", "make --old-file=test build", "go test 'unterminated", "go test \"unterminated", "bash -c 'go vet ./...' go test",
	"npm -w test build", "npx --yes cowsay", "npx -p test cowsay", "python3 -c 'import os' -m pytest", "python3 -W ignore script.py",
	"python3 -W -m pytest x", "pnpm --filter test build", "bash -c 'go test ./...", "bash -c \"make test",
	"bash -c 'go vet ./...'", "eval 'go vet ./...'", "echo $(go list ./...)", "go vet ./... # don't test", "npm exec eslint", "",
	"bash -l script.sh", "echo \"$(go list ./...)\"", "echo '$(go test ./...)'", "yarn workspace app build", "cargo build", "python -m venv test",
	"echo 'unterminated", "echo \"unterminated", strings.Repeat("a b ", 1<<18),
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

// A leaf's test run is blocked; the main session and every mention of a test word pass.
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
		if r := run(c, "a1"); r.Decision != core.Block || r.Reason != wholeTreeReason {
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
	if r := run("go test ./internal/x", ""); r.Decision != core.Block {
		t.Errorf("dispatched agent by env: got %v, want block", r.Decision)
	}
	scan := `{"tool_name":"Bash","agent_id":"a1","tool_input":{"command":"make test"}}`
	if r := scanPayload(strings.NewReader(scan)); r.Decision != core.Block {
		t.Errorf("Scan leaf make test: got %v, want block", r.Decision)
	}
}
