package ask

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
)

func TestMemoryRunOptionsNeverFallBack(t *testing.T) {
	o := memoryRunOptions(context.Background(), "p", "/w")
	if o.Fallback != router.FallbackNone || o.Agent != "claude" || !o.Tools || o.WorkDir != "/w" {
		t.Fatalf("memoryRunOptions = %+v, want claude with tools and FallbackNone", o)
	}
	if o.Runner != agentconfig.RunnerOwn {
		t.Fatalf("Runner = %q, want own so Pi never leads a memory job", o.Runner)
	}
}

func TestMemoryHopOptionsAgreeOnTarget(t *testing.T) {
	base := memoryRunOptions(context.Background(), "p", "/w")
	base.Lead, base.Account = "dev", "acct"
	o := hopOptions(base, route.Route{Provider: "databricks", Profile: "qa"})
	if o.Provider != "databricks:qa" || o.Route.Profile != "qa" || o.Lead != "qa" || o.Account != "" || o.Runner != agentconfig.RunnerOwn {
		t.Fatalf("hopOptions = %+v, want one qa target with the own runner", o)
	}
}

// A pin auth failure followed by a progressed, non-auth failure on a later hop is that later failure's receipt.
func TestMemoryWalkClassifiesTerminalAttempt(t *testing.T) {
	ws := func(n string) *providers.Provider {
		return &providers.Provider{Name: n, Provider: providers.GroupDatabricks}
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"dev": ws("dev"), "qa": ws("qa"), "prod": ws("prod")}}
	ctx, rep := memory.WithJobReport(context.Background())
	var ran []string
	run := func(_ context.Context, o agent.RunOptions) (wire.Result, error) {
		ran = append(ran, o.Provider)
		return wire.Result{Agent: "claude"}, &retry.Error{Kind: retry.KindTransient, Progressed: true}
	}
	pin := attempt{res: wire.Result{Agent: "claude"}, profile: "dev", err: providers.ErrNeedsLogin}
	if _, err := memoryWalk(ctx, run, reg, memoryRunOptions(ctx, "p", "/w"), pin); err == nil {
		t.Fatal("walk error = nil, want the joined failures")
	}
	if len(ran) != 1 || ran[0] != "databricks:prod" && ran[0] != "databricks:qa" {
		t.Fatalf("hops run = %v, want one sibling then stop on progress", ran)
	}
	if rep.Class != memory.JobClassFailed || rep.Fix != "" || rep.Profile != strings.TrimPrefix(ran[0], "databricks:") {
		t.Fatalf("report = %+v, want a retryable failed receipt on the terminal hop", *rep)
	}
}

// An ask brief gains one recall block, keeps a held one unchanged, and a retry replays the sent bytes.
func TestAskAttachesRecallOnce(t *testing.T) {
	dir := t.TempDir()
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "HOME", "PATH"} {
		t.Setenv(k, dir)
	}
	task := "stage the deploy pipeline staging signoff rollout " + strings.Repeat("and verify the result ", 10)
	run := func(args ...string) string {
		hosttest.CaptureOutput(t, func() int {
			return Cmd(append([]string{"--agent", "claude", "--tools", "--fallback", "none", "--format", "json"}, args...))
		})
		return agent.RecentRecords(1)[0].Request.Prompt
	}
	sent := task + "\n\n" + agent.ReviewLoopRule + "\n"
	if bare := run(task); bare != sent {
		t.Fatalf("empty bank: prompt = %q, want the task plus the review rule", bare)
	}
	bareID := agent.RecentRecords(1)[0].ID
	var b strings.Builder
	for i := range 3 {
		fmt.Fprintf(&b, "- deploy pipeline staging signoff rule %d <!-- id:%08x -->\n", i, i+1)
	}
	bank := filepath.Join(paths.MemoryStoreDir(), "conventions.md")
	if err := os.MkdirAll(filepath.Dir(bank), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bank, []byte("# Conventions\n\n"+b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	first := run(task)
	if strings.Count(first, "<memory-recall>") != 1 || !strings.HasPrefix(first, task) {
		t.Fatalf("prompt = %q, want the task plus one block", first)
	}
	id := agent.RecentRecords(1)[0].ID
	if held := run(first); held != first {
		t.Errorf("a brief holding a block changed: %q", held)
	}
	if again := run("--retry", id); again != first {
		t.Errorf("retry sent %q, want %q", again, first)
	}
	if again := run("--retry", bareID); again != sent {
		t.Errorf("retry of a bare send = %q, want %q", again, sent)
	}
}

// A catchable kill cancels the memory run's context, and the receipt names the signal.
func TestMemorySignalsCancelAndNameTheKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no SIGHUP delivery on windows")
	}
	ctx, stop := memorySignals(t.Context())
	self, _ := os.FindProcess(os.Getpid())
	if err := self.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not cancel the memory run")
	}
	sig := stop()
	if sig != syscall.SIGHUP {
		t.Fatalf("stop() = %v, want SIGHUP", sig)
	}
	got := memoryReceipt(7, "", 1500*time.Millisecond, sig, ctx.Err())
	if want := "memory job: pid 7 dispatch - ended after 1.5s: error: context canceled (signal hangup)"; got != want {
		t.Errorf("receipt = %q, want %q", got, want)
	}
	if _, stop := memorySignals(t.Context()); stop() != nil {
		t.Error("stop() with no signal named one")
	}
}

func TestMemoryHopsEndWithLocal(t *testing.T) {
	ws := func(n string) *providers.Provider {
		return &providers.Provider{Name: n, Provider: providers.GroupDatabricks}
	}
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close()
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"dev": ws("dev"), "qa": ws("qa"), "prod": ws("prod"),
		"local": {Name: "local", Provider: "magnitude", BaseURL: url, Models: map[string]string{"sonnet": "q"}},
	}}
	got := memoryHops(reg, "dev")
	if len(got) != 3 || got[2] != localRoute || got[0].Profile == "dev" || got[1].Profile == "dev" {
		t.Fatalf("hops for dev = %v, want the other two workspaces then local", got)
	}
	if got := memoryHops(reg, "work"); len(got) != 1 || got[0] != (route.Route{Provider: "magnitude", Profile: "local"}) {
		t.Fatalf("hops for a non-Databricks pin = %v, want [local]", got)
	}
	start := time.Now()
	if localUp(context.Background(), reg) || time.Since(start) > localProbe+time.Second {
		t.Fatalf("localUp against a closed port = true or slow (%s)", time.Since(start))
	}
	checkJobClasses(t, reg)
}

func checkJobClasses(t *testing.T, reg *providers.Registry) {
	t.Helper()
	for _, tc := range []struct {
		err       error
		class, fx string
	}{
		{providers.ErrNeedsLogin, memory.JobClassAuth, "claude-code databricks login --profile dev"},
		{fmt.Errorf("x: %w", agent.ErrTreeBudget), memory.JobClassBudget, "claude-code memory update --session " + memory.FixSessionSlot},
		{errors.New("boom"), memory.JobClassFailed, ""},
	} {
		if c, fix := jobClass(reg, "dev", tc.err); c != tc.class || fix != tc.fx {
			t.Fatalf("jobClass(%v) = %q %q, want %q %q", tc.err, c, fix, tc.class, tc.fx)
		}
	}
}

// The persisted command is usable verbatim; the recovery guidance is a separate log note.
func TestMemoryBudgetReceiptCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv(memory.UpdateChildEnv, "1") // no background embedding refresh
	const session = "2bc8f390-6f7a-4acd-8209-ab02de2d1f87"
	transcript := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var note bytes.Buffer
	run := func(ctx context.Context, _, _ string) (string, error) {
		err := fmt.Errorf("spent: %w", agent.ErrTreeBudget)
		reportFailure(memory.WithJobOutput(ctx, &note), &providers.Registry{}, attempt{err: err})
		return "claude", err
	}
	err := memory.UpdateSession(memory.UpdateSessionOptions{CWD: dir, Session: session, Transcript: transcript, Run: run, Out: io.Discard})
	if !errors.Is(err, agent.ErrTreeBudget) {
		t.Fatalf("update error = %v, want tree budget", err)
	}
	data, err := os.ReadFile(filepath.Join(paths.MemoryStateDir(), "update-results", "update-result-"+session))
	if err != nil {
		t.Fatal(err)
	}
	want := "claude-code memory update --session " + session
	var fix string
	for line := range strings.SplitSeq(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "fix="); ok {
			fix = value
		}
	}
	if fix != want {
		t.Fatalf("persisted fix = %q, want %q", fix, want)
	}
	if !strings.Contains(note.String(), "retry outside the spent dispatch tree, or after raising the cap") {
		t.Fatalf("budget note = %q", note.String())
	}
	checkBudgetFixCommand(t, dir, fix, session)
}

// Use a fake executable so the printed line can be run without touching a real store.
func checkBudgetFixCommand(t *testing.T, dir, fix, session string) {
	t.Helper()
	name, script := "claude-code", "#!/bin/sh\nprintf '%s\\n' \"$@\"\n"
	if runtime.GOOS == "windows" {
		name, script = "claude-code.cmd", "@echo off\r\necho %1\r\necho %2\r\necho %3\r\necho %4\r\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// Use the system shell directly; do not rely on the optional ComSpec or PATHEXT settings.
		shell := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
		cmd = exec.Command(shell, "/d", "/c", fix)
	} else {
		cmd = exec.Command("/bin/sh", "-c", fix)
	}
	cmd.Env = append(os.Environ(), "PATH="+dir, "PATHEXT=.CMD;.EXE")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.ReplaceAll(string(out), "\r\n", "\n") != "memory\nupdate\n--session\n"+session+"\n" {
		t.Fatalf("running fix: output %q, error %v", out, err)
	}
}

func TestMemoryRunOptionsSchemaTurnsToolsOff(t *testing.T) {
	ctx, text := memory.WithJobAnswer(context.Background(), `{"type":"object"}`)
	o := memoryRunOptions(ctx, "p", "/w")
	if o.Tools || o.OutputSchema != `{"type":"object"}` {
		t.Fatalf("memoryRunOptions = %+v, want tools off and the schema set", o)
	}
	ws := func(n string) *providers.Provider {
		return &providers.Provider{Name: n, Provider: providers.GroupDatabricks}
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"dev": ws("dev"), "qa": ws("qa")}}
	run := func(context.Context, agent.RunOptions) (wire.Result, error) {
		return wire.Result{Agent: "claude", Structured: []byte(`{"ops":[]}`)}, nil
	}
	if _, err := memoryWalk(ctx, run, reg, o, attempt{err: errors.New("x"), profile: "dev"}); err != nil {
		t.Fatal(err)
	}
	if *text != `{"ops":[]}` {
		t.Fatalf("answer slot = %q, want the structured answer", *text)
	}
	if plain := memoryRunOptions(context.Background(), "p", "/w"); !plain.Tools || plain.OutputSchema != "" {
		t.Fatalf("no slot: %+v, want tools on and no schema", plain)
	}
}
