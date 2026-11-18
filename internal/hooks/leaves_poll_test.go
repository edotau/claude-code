package hooks

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
)

const pollJobID = "20261116T120000.000-opencode"

// relayTx writes opencode leaf a1's transcript where the session's hooks find it and returns the session transcript.
func relayTx(t *testing.T, lines ...string) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	sess := filepath.Join(t.TempDir(), "sess.jsonl")
	dir := filepath.Join(strings.TrimSuffix(sess, ".jsonl"), "subagents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	launch := []string{txCall("t0", "Bash", "claude-code agents ask --agent opencode --background - < b.md"),
		txResult("t0", "job "+pollJobID+" started")}
	body := strings.Join(append(launch, lines...), "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "agent-a1.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-a1.meta.json"), []byte(`{"agentType":"opencode"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return sess
}

func txCall(id, tool, command string) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]any{
		{"type": "tool_use", "id": id, "name": tool, "input": map[string]any{"command": command}}}}})
	return string(b)
}

func txResult(id, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{
		{"type": "tool_result", "tool_use_id": id, "content": text}}}})
	return string(b)
}

// bashHook is leaf a1's Bash hook payload for one call; an empty agent is the main session's.
func bashHook(event, sess, agent, toolUseID, command, stdout string) *core.Input {
	b, _ := json.Marshal(map[string]any{"hook_event_name": event, "tool_name": "Bash", "agent_id": agent, "transcript_path": sess,
		"tool_use_id": toolUseID, "tool_input": map[string]any{"command": command}, "tool_response": map[string]any{"stdout": stdout}})
	return core.ParseInput(strings.NewReader(string(b)))
}

// (a) A relay's own poll that returns on an ended job is told to hand back; a running job or the main session is not.
func TestPollNudgeOnEndedJob(t *testing.T) {
	sess := relayTx(t)
	poll := "timeout 590 claude-code agents result -f 20261116T120000.000 2>&1 | tail -60"
	writeDispatch(t, pollJobID, record(pollJobID, "running", os.Getpid()))
	if r := AfterTool(bashHook("PostToolUse", sess, "a1", "t1", poll, "still going")); r.Context != "" {
		t.Fatalf("running job: context %q, want none", r.Context)
	}
	writeDispatch(t, pollJobID, record(pollJobID, "ok", 0))
	if r := AfterTool(bashHook("PostToolUse", sess, "", "t1", poll, "done")); r.Context != "" {
		t.Fatalf("main session: context %q, want none", r.Context)
	}
	want := "job " + pollJobID + " ended: hand back now with this result"
	if r := AfterTool(bashHook("PostToolUse", sess, "a1", "t1", poll, "answer\nask: ran on opencode · x")); r.Context != want {
		t.Fatalf("ended job: context %q, want %q", r.Context, want)
	}
}

// (b) After a delivered hand-back, a poll of the same finished job is refused with "already reported: stop".
func TestPollGateAlreadyReported(t *testing.T) {
	poll := "claude-code agents result -f " + pollJobID
	polled := []string{txCall("t1", "Bash", poll), txResult("t1", "answer\nask: ran on opencode · x")}
	handback := []string{txCall("h1", "SubagentHandback", ""), txResult("h1", `{"success":true,"message":"Report delivered to your caller."}`)}
	sess := relayTx(t, polled...)
	writeDispatch(t, pollJobID, record(pollJobID, "ok", 0))
	if r := Handback(bashHook("PreToolUse", sess, "a1", "t2", poll, "")); r.Decision == core.Block {
		t.Fatalf("not yet handed back: %+v, want the poll allowed", r)
	}
	sess = relayTx(t, append(polled, handback...)...)
	writeDispatch(t, pollJobID, record(pollJobID, "ok", 0))
	r := Handback(bashHook("PreToolUse", sess, "a1", "t2", poll, ""))
	if r.Decision != core.Block || !strings.HasPrefix(r.Reason, "already reported: stop") {
		t.Fatalf("handed back: %+v, want a block starting \"already reported: stop\"", r)
	}
}

// (c) A second follow poll of one job is refused while the first is in flight or backgrounded with a live writer.
func TestPollGateOnePoller(t *testing.T) {
	poll := "claude-code agents result -f " + pollJobID
	out := bgOutputFile(t, "")
	holders := []int{}
	orig := reapHolders
	t.Cleanup(func() { reapHolders = orig })
	reapHolders = func(files []string) []int {
		if len(files) == 1 && files[0] == out {
			return holders
		}
		return nil
	}
	for _, c := range []struct {
		name    string
		lines   []string
		holders []int
		block   bool
		command string
	}{
		{"HIGH1 output redirect", nil, nil, true, "claude-code agents ask x > /some/file.go"},
		{"in flight", []string{txCall("t1", "Bash", poll)}, nil, true, ""},
		{"HIGH4 nested wrappers", []string{txCall("t1", "Bash", "rtk env -i timeout -k 2 30 rtk "+poll)}, nil, true, ""},
		{"in flight, rtk-prefixed", []string{txCall("t1", "Bash", "rtk "+poll)}, nil, true, ""},
		{"backgrounded, writer alive", []string{txCall("t1", "Bash", poll), txResult("t1", bgNotice(out))}, []int{4242}, true, ""},
		{"backgrounded, writer gone", []string{txCall("t1", "Bash", poll), txResult("t1", bgNotice(out))}, nil, false, ""},
		{"returned", []string{txCall("t1", "Bash", poll), txResult("t1", "▸ heartbeat: x")}, nil, false, ""},
	} {
		sess := relayTx(t, c.lines...)
		writeDispatch(t, pollJobID, record(pollJobID, "running", os.Getpid()))
		holders = c.holders
		command := poll
		if c.command != "" {
			command = c.command
		}
		r := Handback(bashHook("PreToolUse", sess, "a1", "t2", command, ""))
		if got := r.Decision == core.Block && (strings.HasPrefix(r.Reason, "one poller only") || c.command != ""); got != c.block {
			t.Errorf("%s: %+v, want block=%v", c.name, r, c.block)
		}
	}
}

func TestRelaySyntax(t *testing.T) {
	for _, cmd := range []string{
		"claude-code agents ask x > /some/file.go",
		"claude-code agents ask $(printf x)",
		"claude-code agents ask `printf x`",
		"claude-code agents ask <(printf x)",
		"claude-code agents ask x 2>&1",
	} {
		sess := relayTx(t)
		if r := Handback(bashHook("PreToolUse", sess, "a1", "t2", cmd, "")); r.Decision != core.Block {
			t.Errorf("HIGH1 %q allowed: %+v", cmd, r)
		}
	}
	r := Handback(bashHook("PreToolUse", relayTx(t), "a1", "t2", "claude-code agents ask $(printf x)", ""))
	if want := "refused `claude-code agents ask $(printf x)`: contains `$` or a backtick"; !strings.Contains(r.Reason, want) {
		t.Errorf("reason %q, want it to contain %q", r.Reason, want)
	}
}

// A relay may run read-only commands and go gates; edits, other runners and a `cd` stay refused.
func TestRelayReadOnlyAndGates(t *testing.T) {
	cases := map[string]bool{
		"rtk grep -n foo internal/hooks/x.go": true, "grep -rn foo internal": true, "git diff": true,
		"git -C /Users/edotau/.claude diff --stat": true, "git show 1ca08500 --stat": true,
		"go test -count=1 -run TestCancelDetectsPIDReuse ./internal/agent/": true, "rtk go test ./internal/hooks/": true,
		"go test ./... | tail -20": true, "go build ./... && go vet ./internal/hooks/": true, "gofmt -l internal/hooks/x.go": true,
		"sed -i s/a/b/ f": false, "echo x > f": false, "git commit -m x": false, "git add f": false, "git stash": false,
		"go run ./cmd/x": false, "go test -exec ./x ./...": false, "go build -o /tmp/x ./cmd/claude-code": false,
		"make test": false, "rm f": false, `python3 -c "print(1)"`: false, `bash -c "ls"`: false, "gofmt -w f.go": false,
		"cd /x && grep y z": false, "go generate ./...": false, "GOFLAGS=-toolexec=x go test ./...": false,
	}
	for cmd, want := range cases {
		r := Handback(bashHook("PreToolUse", relayTx(t), "a1", "t2", cmd, ""))
		if got := r.Decision != core.Block; got != want {
			t.Errorf("%q allowed=%v, want %v: %s", cmd, got, want, r.Reason)
		}
	}
}

// A relay's go gate is an explicit shape: packages and a short flag list, nothing that writes or runs a program.
func TestRelayGoGateShapes(t *testing.T) {
	cases := map[string]bool{
		"go test ./internal/x/ -run TestY -count=1": true, "go test -race -short -timeout 2m -failfast -shuffle on ./...": true,
		"go test -v -tags integration -cover -list . ./internal/x": true, "go vet ./...": true, "go vet -tags x ./a ./b": true,
		"go build ./...": true, "go build ./a ./b": true, "go build -o /dev/null ./cmd/claude-code": true, "gofmt -l file.go": true,
		"ls internal": true, "go doc fmt": true,
		"go test ./x -args -test.cpuprofile=/outside/file": false, "go test ./x -args": false, "GOFLAGS=-toolexec=x go test ./...": false,
		"GOENV=/x go vet ./...": false, "GOCACHE=/x go build ./...": false, "env GOFLAGS=-exec=x go test ./...": false,
		"go build ./cmd/claude-code": false, "go build": false, "go build -o /tmp/x ./...": false, "go test -c ./x": false,
		"go test -exec ./x ./...": false, "go test -toolexec=x ./...": false, "go test -overlay=o.json ./...": false,
		"go vet -vettool=./x ./...": false, "go test -o /tmp/x ./x": false, "go test -coverprofile=c.out ./x": false,
		"go test -cpuprofile cpu.out ./x": false, "go test -memprofile=m ./x": false, "go test -trace=t ./x": false,
		"go test -json ./x": false, "go test -modfile=x.mod ./x": false, "go test --exec=x ./x": false,
		"gofmt -w f.go": false, "gofmt -d f.go": false, "gofmt -l -w f.go": false, "gofmt f.go": false,
	}
	for cmd, want := range cases {
		r := Handback(bashHook("PreToolUse", relayTx(t), "a1", "t2", cmd, ""))
		if got := r.Decision != core.Block; got != want {
			t.Errorf("%q allowed=%v, want %v: %s", cmd, got, want, r.Reason)
		}
	}
	t.Setenv("GOFLAGS", "-mod=readonly -toolexec=/x/y")
	r := Handback(bashHook("PreToolUse", relayTx(t), "a1", "t2", "go test ./...", ""))
	if r.Decision != core.Block || !strings.Contains(r.Reason, "inherited GOFLAGS") || !strings.Contains(r.Reason, "go test <packages>") {
		t.Errorf("inherited GOFLAGS: %+v, want a block naming it and the allowed shapes", r)
	}
	t.Setenv("GOFLAGS", "-mod=readonly")
	if r := Handback(bashHook("PreToolUse", relayTx(t), "a1", "t2", "go test ./...", "")); r.Decision == core.Block {
		t.Errorf("harmless GOFLAGS: %+v, want allowed", r)
	}
}

// A relay's own successful `agents result|wait <id>` after a backgrounded ask binds an id whose record is its ask's.
func TestHandbackBindsOwnPoll(t *testing.T) {
	const id = "20261117T101000.000-opencode"
	const owner = 4242
	oldOwner := watchOwner
	t.Cleanup(func() { watchOwner = oldOwner })
	watchOwner = func() int { return owner } // a fixed session pid: the ambient one is 0 outside a Claude session
	at := time.Now().Add(-15 * time.Minute).Truncate(time.Second)
	good := "▸ result: " + id + "\nANSWER\nask: ran on opencode · m"
	res, wait := "claude-code agents result -f "+id, "claude-code agents wait "+id+" --timeout 590s"
	for _, c := range []pollRow{
		{name: "result -f", cmd: res, want: true},
		{name: "wait", cmd: wait, want: true},
		{name: "brief file ask", cmd: res, file: true, want: true},
		{name: "brief file of another prompt", cmd: res, file: true, prompt: "audit beta"},
		{name: "rtk wait", cmd: "rtk " + wait, want: true},
		{name: "tool error", cmd: res, isErr: true},
		{name: "hook denial text", cmd: res, out: "PreToolUse:Bash hook error: blocked"},
		{name: "permissionDecision deny", cmd: res, out: `{"permissionDecision":"deny"}`},
		{name: "no record", cmd: res, noRec: true},
		{name: "another session's record", cmd: res, own: owner + 1},
		{name: "record owner 0", cmd: res, own: -1},
		{name: "record before the ask", cmd: res, start: -time.Second},
		{name: "poll with no prior ask", cmd: res, noAsk: true},
		{name: "launched after the window", cmd: res, start: askLaunchWindow + time.Second},
		{name: "sibling's prompt", cmd: res, prompt: "audit beta"},
		{name: "sibling's agent", cmd: res, agent: "codex"},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.runPollRow(t, id, good, at, owner)
		})
	}
}

// pollRow is one case of TestHandbackBindsOwnPoll; a zero field is the leaf's own ask's value.
type pollRow struct {
	name, cmd, out, agent, prompt string
	isErr, noRec, want, file      bool
	noAsk                         bool
	own                           int
	start                         time.Duration
}

func (c pollRow) runPollRow(t *testing.T, id, good string, at time.Time, owner int) {
	sess := relayTx(t, "")
	out := cmp.Or(c.out, good)
	ask := `claude-code agents ask --agent opencode "audit alpha"`
	if c.noAsk {
		ask = ""
	}
	if c.file {
		brief := filepath.Join(t.TempDir(), "brief.md")
		if err := os.WriteFile(brief, []byte("audit alpha\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ask = "claude-code agents ask --agent opencode --background - < " + brief
	}
	tx := filepath.Join(strings.TrimSuffix(sess, ".jsonl"), "subagents", "agent-a1.jsonl")
	if err := os.WriteFile(tx, []byte(pollTranscript(t, at, ask, c.cmd, out, c.isErr)), 0o600); err != nil {
		t.Fatal(err)
	}
	if !c.noRec {
		writeDispatch(t, id, fmt.Sprintf(`{"id":%q,"time":%q,"agent":%q,"status":"ok","owner":%d,"model":"m","log":%q,"request":{"Prompt":%q}}`,
			id, at.Add(cmp.Or(c.start, time.Second)).Format(time.RFC3339), cmp.Or(c.agent, "opencode"), max(cmp.Or(c.own, owner), 0),
			writeJobFiles(t, id, good+"\n", ""), cmp.Or(c.prompt, "audit alpha")))
	}
	if got := readLeaf(tx, leafWant{ids: true}).ids; slices.Contains(got, id) != c.want {
		t.Errorf("ids %v, want %s bound=%v", got, id, c.want)
	}
	if c.noAsk {
		return // with no ask at all the hand-back passes as relay work, not by this binding
	}
	r := Handback(handbackOf(leafInput(tx), "relay report"))
	if r.Decision == core.Block {
		t.Errorf("hand-back %+v, want it accepted (bound=%v)", r, c.want)
	}
}

// pollTranscript is a relay's timed transcript: a backgrounded ask at `at`, then the poll `cmd` ten minutes later.
func pollTranscript(t *testing.T, at time.Time, ask, cmd, out string, isErr bool) string {
	t.Helper()
	msg := func(typ string, ts time.Time, content any) string {
		line, _ := json.Marshal(map[string]any{"type": typ, "timestamp": ts, "message": map[string]any{"content": content}})
		return string(line) + "\n"
	}
	use := func(id, cmd string, ts time.Time) string {
		return msg("assistant", ts, []map[string]any{{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": cmd}}})
	}
	res := func(id, text string, isErr bool) string {
		return msg("user", at, []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": text, "is_error": isErr}})
	}
	launch := use("t1", ask, at) + res("t1", bgNotice(bgOutputFile(t, "working\n")), false)
	if ask == "" {
		launch = ""
	}
	return msg("user", at.Add(-time.Minute), "Objective: x\nAgent: opencode\n") + launch +
		use("t2", cmd, at.Add(10*time.Minute)) + res("t2", out, isErr)
}
