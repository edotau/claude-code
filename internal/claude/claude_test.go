package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/agenttest"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/network/egress"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const claudeFixture = `{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-opus-5","tools":["Bash"]}
{"type":"system","subtype":"hook_started","session_id":"sess-1"}
{"type":"assistant","session_id":"sess-1","parent_tool_use_id":null,"message":{"model":"claude-opus-5","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Hello"}]}}
{"type":"assistant","session_id":"sess-1","parent_tool_use_id":"toolu_1","message":{"content":[{"type":"text","text":"subagent chatter"}]}}
{"type":"user","session_id":"sess-1","message":{"content":[{"type":"tool_result","content":"x"}]}}
{"type":"stream_event","session_id":"sess-1","event":{}}
{"type":"assistant","session_id":"sess-1","message":{"content":[{"type":"tool_use","name":"Bash"},{"type":"text","text":"world"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"Hello\nworld","session_id":"sess-1","num_turns":2,"total_cost_usd":0.01,"usage":{"input_tokens":10,"cache_read_input_tokens":90,"output_tokens":4}}
`

// parseClaudeStream reads a whole stream with no idle watch.
func parseClaudeStream(br *bufio.Reader, sink wire.Sink, logs ...wire.StreamLog) (wire.Result, error) {
	return parseClaudeEvents(br, false, nil, sink, nil, logs...)
}

func TestParseClaudeStream(t *testing.T) {
	var out strings.Builder
	res, err := parseClaudeStream(newBR(claudeFixture), wire.NewTextSink(&out))
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "Hello\nworld" {
		t.Errorf("stream = %q (subagent text must be skipped)", out.String())
	}
	if res.Answer != "Hello\nworld" || res.SessionID != "sess-1" || res.Model != "claude-opus-5" || res.Usage != (wire.Usage{InputTokens: 10, CacheReadTokens: 90, OutputTokens: 4, CostUSD: 0.01}) {
		t.Errorf("res = %+v", res)
	}
}

func TestParseClaudeStreamEmptyResultKeepsText(t *testing.T) {
	stream := `{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"}]}}
{"type":"result","is_error":false,"result":""}`
	var out strings.Builder
	res, err := parseClaudeStream(newBR(stream), wire.NewTextSink(&out))
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "Hello" || res.Answer != "Hello" {
		t.Fatalf("output = %q, Answer = %q; want Hello in both", out.String(), res.Answer)
	}
}

func TestParseClaudeStreamErrors(t *testing.T) {
	isErr := `{"type":"result","subtype":"success","is_error":true,"result":"API Error: 429 rate_limit_error"}` + "\n"
	_, err := parseClaudeStream(newBR(isErr), wire.NewTextSink(&strings.Builder{}))
	if e := retry.Classify("claude", err, false); e == nil || e.Kind != retry.KindRateLimit {
		t.Errorf("is_error result = %v", err)
	}
	if _, err := parseClaudeStream(newBR(`{"type":"system","subtype":"init"}`), wire.NewTextSink(&strings.Builder{})); err == nil {
		t.Error("a stream without a result event must fail")
	}
	if _, err := parseClaudeStream(newBR(`{"type":`), wire.NewTextSink(&strings.Builder{})); err == nil {
		t.Error("truncated JSON must fail")
	}
}

func TestParseClaudeStreamResultApplication(t *testing.T) {
	stream := `{"type":"system","session_id":"init-id","model":"init-model"}
	{"type":"assistant","message":{"model":"answer-model","content":[{"type":"text","text":""},{"type":"thinking","text":"hidden"},{"type":"text","text":"one"},{"type":"text","text":"two"}]}}
	{"type":"assistant","parent_tool_use_id":"tool-1","message":{"model":"subagent-model","content":[{"type":"text","text":"hidden"}]}}
	{"type":"result","session_id":"result-id","is_error":true,"subtype":"error_during_execution","result":"partial result","usage":{"input_tokens":7,"output_tokens":3,"cache_creation_input_tokens":2,"cache_read_input_tokens":4}}`
	var out strings.Builder
	res, err := parseClaudeStream(newBR(stream), wire.NewTextSink(&out))
	if err == nil || err.Error() != "claude error_during_execution: partial result" {
		t.Fatalf("err = %v", err)
	}
	if out.String() != "one\ntwo" || res.Answer != "one\n\ntwo" || res.SessionID != "result-id" || res.Model != "answer-model" {
		t.Fatalf("res = %+v, output = %q", res, out.String())
	}
	if res.Usage != (wire.Usage{InputTokens: 7, OutputTokens: 3, CacheCreationTokens: 2, CacheReadTokens: 4}) {
		t.Fatalf("usage = %+v", res.Usage)
	}
}

func TestParseClaudeStreamWriteError(t *testing.T) {
	res, err := parseClaudeStream(newBR(claudeFixture), wire.NewTextSink(failingStreamWriter{}))
	if !errors.Is(err, io.ErrClosedPipe) || res.SessionID != "sess-1" || res.Model != "claude-opus-5" || res.Answer != "Hello" { // parsed text survives the dead sink
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

type newlineRejectWriter struct {
	writes []string
	err    error
}

func (w *newlineRejectWriter) Write(p []byte) (int, error) {
	w.writes = append(w.writes, string(p))
	if string(p) == "\n" {
		return 0, w.err
	}
	return len(p), nil
}

func TestParseClaudeStreamNewlineError(t *testing.T) {
	for name, stream := range map[string]string{
		"blocks": `{"type":"assistant","message":{"content":[{"type":"text","text":"one"},{"type":"text","text":"two"},{"type":"text","text":"three"}]}}
{"type":"result","result":"one\ntwo\nthree"}`,
		"events": `{"type":"assistant","message":{"content":[{"type":"text","text":"one"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"two"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"three"}]}}
{"type":"result","result":"one\ntwo\nthree"}`,
	} {
		t.Run(name, func(t *testing.T) {
			injected := errors.New("newline rejected")
			w := &newlineRejectWriter{err: injected}
			_, err := parseClaudeStream(newBR(stream), wire.NewTextSink(w))
			if !errors.Is(err, injected) {
				t.Fatalf("err = %v, want injected newline error", err)
			}
			if len(w.writes) != 2 || w.writes[0] != "one" || w.writes[1] != "\n" {
				t.Fatalf("writes = %q, want only first text and rejected newline", w.writes)
			}
		})
	}
}

// permissionDeniedEvent is sampled from the real claude CLI binary (2.1.304, strings -a dump of the
// installed `claude` bundle): a "system","permission_denied" event carries "message" as a plain
// string, not the {model, content[]} object every assistant/user event uses. It is the real shape
// behind the production failure recorded in state/harness/dispatch (decode stream-json: json: cannot
// unmarshal string into Go struct field claudeEvent.message).
const permissionDeniedEvent = `{"type":"system","subtype":"permission_denied","tool_name":"Bash","tool_use_id":"toolu_1","agent_id":"","decision_reason_type":"rule","decision_reason_code":"deny","decision_reason":"blocked by settings","message":"Bash is not allowed in this session","session_id":"sess-2"}`

// TestParseClaudeStreamStringMessage is the RED fixture: one odd event (a string "message") must
// never abort decoding of the rest of a real stream (a normal assistant turn plus the result event).
func TestParseClaudeStreamStringMessage(t *testing.T) {
	stream := permissionDeniedEvent + "\n" +
		`{"type":"assistant","session_id":"sess-2","message":{"model":"claude-opus-5","content":[{"type":"text","text":"done"}]}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"sess-2"}` + "\n"
	var out strings.Builder
	res, err := parseClaudeStream(newBR(stream), wire.NewTextSink(&out))
	if err != nil {
		t.Fatalf("a string message event must not fail the dispatch: %v", err)
	}
	if out.String() != "done" || res.Answer != "done" || res.SessionID != "sess-2" {
		t.Fatalf("res = %+v, output = %q", res, out.String())
	}
}

// TestParseClaudeStreamCountsGateBlocks counts is_error tool_results carrying the gate's prefix (not a live capture).
func TestParseClaudeStreamCountsGateBlocks(t *testing.T) {
	user := func(parent, results string) string {
		return `{"type":"user","parent_tool_use_id":` + parent + `,"message":{"role":"user","content":[` + results + `]}}` + "\n"
	}
	str := `{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"permission-gate: BLOCKED — Edit denied"}`
	parts := `{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":[{"type":"text","text":"permission-gate: BLOCKED — x"}]}`
	plain := `{"type":"tool_result","tool_use_id":"t3","is_error":true,"content":"File not found"}`
	okGate := `{"type":"tool_result","tool_use_id":"t4","is_error":false,"content":"permission-gate: BLOCKED quoted by a file"}`
	stream := user("null", str+","+parts+","+plain+","+okGate) + user(`"toolu_9"`, str) +
		`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s"}` + "\n"
	res, err := parseClaudeStream(newBR(stream), wire.NewTextSink(io.Discard))
	if err != nil || res.GateBlocks != 2 {
		t.Fatalf("GateBlocks = %d, err = %v, want 2 (string + parts; plain error, non-error and subagent ignored)", res.GateBlocks, err)
	}
}

// TestParseClaudeStreamErrorEventText surfaces an "error"-type event's string message as the returned error.
func TestParseClaudeStreamErrorEventText(t *testing.T) {
	stream := `{"type":"error","message":"fatal: connection reset","session_id":"sess-3"}` + "\n"
	_, err := parseClaudeStream(newBR(stream), wire.NewTextSink(&strings.Builder{}))
	if err == nil || !strings.Contains(err.Error(), "fatal: connection reset") {
		t.Fatalf("err = %v, want the error event's message text", err)
	}
}

// TestParseClaudeStreamTransport: stream_event timing and api_retry counts land in Transport; answer text is unchanged.
func TestParseClaudeStreamTransport(t *testing.T) {
	stream := `{"type":"stream_event","event":{"type":"message_start"}}` + "\n" +
		`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":500,"error_status":529,"error":"overloaded"}` + "\n" +
		`{"type":"system","subtype":"api_retry","attempt":2,"max_retries":10,"retry_delay_ms":1200,"error_status":null,"error":"timeout"}` + "\n" +
		`{"type":"stream_event","event":{"type":"content_block_delta"}}` + "\n" +
		`{"type":"assistant","message":{"model":"m","content":[{"type":"text","text":"hi"}]}}` + "\n" +
		`{"type":"result","subtype":"success","result":"hi"}` + "\n"
	base := time.Unix(1000, 0)
	ticks := []time.Duration{0, 150 * time.Millisecond, 950 * time.Millisecond} // start, event 1, event 2
	i := 0
	old := wire.Clock
	wire.Clock = func() time.Time { d := ticks[i]; i++; return base.Add(d) }
	defer func() { wire.Clock = old }()
	var out strings.Builder
	var lines []string
	res, err := parseClaudeStream(newBR(stream), wire.NewTextSink(&out), func(k, x string) { lines = append(lines, k+": "+x) })
	if err != nil || res.Answer != "hi" || out.String() != "hi" {
		t.Fatalf("res=%+v out=%q err=%v", res, out.String(), err)
	}
	assertClaudeRetryTransport(t, res.Transport)
	if len(lines) < 1 || lines[0] != "retry: attempt 1/10 status=529 retry_in=500ms" {
		t.Fatalf("log lines = %v", lines)
	}
}

func assertClaudeRetryTransport(t *testing.T, tr *wire.Transport) {
	t.Helper()
	if tr == nil || tr.Retries != 2 || !slices.Equal(tr.RetryStatuses, []int{529, 0}) || tr.FirstEventMs != 150 || tr.MaxGapMs != 800 {
		t.Fatalf("transport = %+v", tr)
	}
}

func newBR(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

type failingStreamWriter struct{}

func (failingStreamWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// Reconstructed from dispatch 20261101T214810.199-claude's log (raw bytes are not kept): a background subagent
// makes the session emit a result, resume, emit another, then close on an empty result.
func TestParseClaudeStreamKeepsLastNonEmptyResult(t *testing.T) {
	stream := `{"type":"assistant","session_id":"s","message":{"content":[{"type":"tool_use","name":"Agent","id":"toolu_1"}]}}
{"type":"result","subtype":"success","is_error":false,"result":"The codex agent is still running.","session_id":"s"}
{"type":"result","subtype":"success","is_error":false,"result":"The dispatch ran and the agent replied PONG-7.","session_id":"s","total_cost_usd":0.2154}
{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"s","total_cost_usd":0.2154}
`
	res, err := parseClaudeStream(newBR(stream), wire.NewTextSink(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	if want := "The dispatch ran and the agent replied PONG-7."; res.Answer != want {
		t.Errorf("Answer = %q, want %q", res.Answer, want)
	}
}

// Assistant text with no result event (the child died mid-turn) is still the observed answer.
func TestParseClaudeStreamNoResultKeepsText(t *testing.T) {
	stream := `{"type":"assistant","session_id":"s","message":{"content":[{"type":"text","text":"Hello"}]}}
{"type":"assistant","parent_tool_use_id":"t","message":{"content":[{"type":"text","text":"sub"}]}}`
	res, err := parseClaudeStream(newBR(stream), wire.Discard)
	if !errors.Is(err, errNoResult) || res.Answer != "Hello" || res.SessionID != "s" {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

// An error result keeps the assistant text already streamed as the answer; the error text rides in the error.
func TestParseClaudeStreamErrorResultKeepsAssistantText(t *testing.T) {
	stream := `{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"}]}}
{"type":"result","is_error":true,"result":"API Error: 529"}`
	res, err := parseClaudeStream(newBR(stream), wire.Discard)
	if err == nil || !strings.Contains(err.Error(), "529") || res.Answer != "Hello" {
		t.Fatalf("res = %+v, err = %v; want Answer Hello and a 529 error", res, err)
	}
}

func TestClaudeArgsPermissions(t *testing.T) {
	rules := func(s ...string) (r []agent.Rule) {
		for _, x := range s {
			p, err := agent.ParseRule(x)
			if err != nil {
				t.Fatal(err)
			}
			r = append(r, p)
		}
		return r
	}
	tail := []string{"--exclude-dynamic-system-prompt-sections"}
	cases := []struct {
		name string
		req  agent.RunOptions
		want []string
	}{
		{"empty keeps today", agent.RunOptions{Tools: true}, tail},
		{"empty autonomous keeps bypass", agent.RunOptions{Tools: true, Autonomous: true}, append([]string{"--permission-mode", "bypassPermissions"}, tail...)},
		{"empty no tools", agent.RunOptions{}, []string{"--tools", "", "--setting-sources", ""}},
		{"allow", agent.RunOptions{Tools: true, Permissions: agent.Permissions{Allow: rules("Bash(git *)", "Edit")}},
			append([]string{"--allowedTools", "Bash(git *)", "Edit", "--permission-mode", "auto", "--permission-prompts", "none"}, tail...)},
		{"deny with auto", agent.RunOptions{Tools: true, Permissions: agent.Permissions{Mode: "auto", Deny: rules("Bash(rm:*)")}},
			append([]string{"--disallowedTools", "Bash(rm:*)", "--permission-mode", "auto", "--permission-prompts", "none"}, tail...)},
		{"ask", agent.RunOptions{Tools: true, Permissions: agent.Permissions{Ask: rules("Write")}},
			append([]string{"--permission-mode", "auto", "--permission-prompts", "none"}, tail...)},
		{"plan denies writes, sets no mode", agent.RunOptions{Tools: true, Permissions: agent.Permissions{Mode: "plan", Ask: rules("Write")}},
			append([]string{"--disallowedTools", "Edit", "Write", "NotebookEdit", "Bash", "--permission-prompts", "none"}, tail...)},
		{"policy replaces bypass", agent.RunOptions{Tools: true, Autonomous: true, Permissions: agent.Permissions{Mode: "auto"}},
			append([]string{"--permission-mode", "auto", "--permission-prompts", "none"}, tail...)},
	}
	for _, c := range cases {
		if got := renderedArgs(t, c.req, anthropicTarget); !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
	err := refuseUnmappedDeny(agent.Permissions{Deny: []agent.Rule{{Arg: "x"}}})
	var ae *retry.Error
	if !errors.As(err, &ae) || ae.Kind != retry.KindFatal || !strings.Contains(err.Error(), "(x)") {
		t.Errorf("nameless deny rule must be refused naming it, got %v", err)
	}
}

// Auto mode's judge calls a fixed Anthropic model a Databricks endpoint does not serve: off Anthropic the rules
// decide alone (dontAsk), so a leaf's Bash/Edit is not refused as "cannot determine the safety".
func TestClaudeArgsPermissionModeByProvider(t *testing.T) {
	hermetic(t, `{"providers":{"databricks":{"qa":{"preset":"databricks","host":"https://qa.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	allow := agent.Permissions{Allow: []agent.Rule{{Tool: "Bash"}}}
	for _, c := range []struct{ profile, want, not string }{{"qa", "dontAsk", "auto"}, {"ultra", "auto", "dontAsk"}} {
		req := agent.RunOptions{Tools: true, Provider: c.profile, Permissions: allow}
		got := strings.Join(renderedArgs(t, req, launchTarget(req)), " ")
		if !strings.Contains(got, "--permission-mode "+c.want) || strings.Contains(got, "--permission-mode "+c.not) {
			t.Errorf("profile %s: args %q, want --permission-mode %s", c.profile, got, c.want)
		}
	}
	// No rules: off Anthropic the leaf allow list replaces settings.json's defaultMode auto; on Anthropic nothing is added.
	tail := []string{"--exclude-dynamic-system-prompt-sections"}
	if got := renderedArgs(t, agent.RunOptions{Tools: true, Provider: "ultra"}, anthropicTarget); !slices.Equal(got, tail) {
		t.Errorf("ultra, no rules: args %q, want %q", got, tail)
	}
	req := agent.RunOptions{Tools: true, Provider: "qa"}
	got := strings.Join(renderedArgs(t, req, launchTarget(req)), " ")
	for _, want := range []string{"--allowedTools Edit Write Read Grep Glob Bash(go build *)", "Bash(go test *)", "Bash(git diff *)",
		"Bash(rtk go test *)", "Bash(rtk read *) --permission-mode dontAsk --permission-prompts none"} {
		if !strings.Contains(got, want) {
			t.Errorf("qa, no rules: args %q, want %q", got, want)
		}
	}
	bypass := agent.RunOptions{Tools: true, Autonomous: true, Provider: "qa"}
	if got := renderedArgs(t, bypass, providers.Target{}); slices.Contains(got, "dontAsk") || !slices.Contains(got, "bypassPermissions") {
		t.Errorf("qa autonomous, no rules: args %q, want bypassPermissions only", got)
	}
}

// The record's effort, permissions and launch env reach a headless claude child through claudeCmd, the flag beats the
// stored effort, and off Anthropic (Databricks included) the mode stays dontAsk because the auto judge is not served.
func TestClaudeCmdRendersRecord(t *testing.T) {
	const dbx = `{"providers":{"databricks":{"qa":{"preset":"databricks","host":"https://qa.cloud.databricks.com","auth":{"type":"pat"}}}}}`
	launch := &vault.AgentLaunch{Env: []string{"FOO_LAUNCH=1", "ANTHROPIC_SMALL_FAST_MODEL=elsewhere"}, Args: []string{"--launch-arg"}}
	cases := []struct {
		name      string
		rec       vault.AgentRecord
		flag      string
		want, not map[string][]string // per profile: argv substrings
	}{
		{"no record", vault.AgentRecord{}, "",
			map[string][]string{"qa": {"--allowedTools Edit Write Read", "--permission-mode dontAsk"}},
			map[string][]string{"ultra": {"--effort", "--permission-mode"}, "qa": {"--effort"}}},
		{"record effort", vault.AgentRecord{Effort: "high"}, "",
			map[string][]string{"ultra": {"--effort high"}, "qa": {"--effort high", "--permission-mode dontAsk"}}, nil},
		{"flag over record", vault.AgentRecord{Effort: "high"}, "low",
			map[string][]string{"ultra": {"--effort low"}, "qa": {"--effort low"}},
			map[string][]string{"ultra": {"--effort high"}, "qa": {"--effort high"}}},
		{"permissions", vault.AgentRecord{Permissions: &vault.AgentPolicy{Allow: []string{"Bash(git *)"}}}, "",
			map[string][]string{"ultra": {"--allowedTools Bash(git *) --permission-mode auto"},
				"qa": {"Bash(go build *)", "Bash(git *) --permission-mode dontAsk"}},
			map[string][]string{"ultra": {"dontAsk"}, "qa": {"auto"}}},
		{"launch", vault.AgentRecord{Launch: launch}, "", nil,
			map[string][]string{"ultra": {"--launch-arg"}, "qa": {"--launch-arg"}}},
	}
	for _, c := range cases {
		for _, prof := range []string{"ultra", "qa"} {
			hermetic(t, dbx)
			if err := vault.SetAgentRecord("claude", c.rec); err != nil {
				t.Fatal(err)
			}
			req := agent.RunOptions{Tools: true, Provider: prof, Effort: c.flag, Launcher: claudeLauncher{}}
			cmd, _, _, err := claudeCmd(context.Background(), req, claudeArgs([]string{"--print"}, req), "")
			if err != nil {
				t.Fatalf("%s/%s: %v", c.name, prof, err)
			}
			checkArgv(t, c.name+"/"+prof, cmd.Args, c.want[prof], c.not[prof])
			if c.rec.Launch != nil && (!slices.Contains(cmd.Env, "FOO_LAUNCH=1") || slices.Contains(cmd.Env, "ANTHROPIC_SMALL_FAST_MODEL=elsewhere")) {
				t.Errorf("%s/%s: env must carry launch.env minus the wiped model name", c.name, prof) // never print cmd.Env
			}
		}
	}
}

// TestClaudeLauncherChildEnvIsRunSnapshot: the launched child inherits the run's snapshot and pinned egress host, not the process env.
func TestClaudeLauncherChildEnvIsRunSnapshot(t *testing.T) {
	hermetic(t, "")
	t.Setenv("FOO_AMBIENT", "ambient")
	run := proc.ValuesFromEnv(os.Environ())
	delete(run, "FOO_AMBIENT")
	ctx := egress.WithHost(proc.WithValues(context.Background(), run.With("FOO_RUN", "snapshot")), "pinned.example")
	cl, err := claudeLauncher{}.Claude(ctx, agent.ClaudeOptions{Provider: "ultra"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"FOO_RUN=snapshot", "HARNESS_EGRESS_HOST=pinned.example"} {
		if !slices.Contains(cl.Env, want) {
			t.Errorf("child env lacks %s", want)
		}
	}
	if slices.Contains(cl.Env, "FOO_AMBIENT=ambient") {
		t.Error("child env carries a value the run snapshot lacks")
	}
}

// checkArgv fails unless the joined argv carries every want substring and no not substring.
func checkArgv(t *testing.T, name string, args, want, not []string) {
	t.Helper()
	argv := strings.Join(args, " ")
	for _, w := range want {
		if !strings.Contains(argv, w) {
			t.Errorf("%s: argv %q, want %q", name, argv, w)
		}
	}
	for _, n := range not {
		if strings.Contains(argv, n) {
			t.Errorf("%s: argv %q must not carry %q", name, argv, n)
		}
	}
}

// anthropicTarget is a first-party Anthropic launch, the one that serves auto mode's judge.
var anthropicTarget = providers.Target{Provider: &providers.Provider{Provider: "anthropic"}}

// renderedArgs is claudeArgs plus claude's RenderSettings for req on t, composed as claudeCmd does.
func renderedArgs(t *testing.T, req agent.RunOptions, tgt providers.Target) []string {
	t.Helper()
	s := agent.Settings{Agent: "claude", Effort: req.Effort, Permissions: req.Permissions, Target: tgt, Tools: req.Tools,
		Autonomous: req.Autonomous, Headless: true}
	_, argv, _, _, err := claudeAgent.RenderSettings(context.Background(), s, nil, claudeArgs(nil, req))
	if err != nil {
		t.Fatal(err)
	}
	return append(argv, toolArgs(req)...)
}

type goldenSink func(wire.Event)

func (f goldenSink) Emit(ev wire.Event) error { f(ev); return nil }

// TestParseClaudeStreamGolden runs a scrubbed real `claude -p --output-format stream-json` capture through the parser.
func TestParseClaudeStreamGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/stream.golden.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var tools []string
	sink := goldenSink(func(ev wire.Event) {
		switch ev.Type {
		case wire.EventText:
			text.WriteString(ev.Text)
		case wire.EventToolUse:
			tools = append(tools, ev.Tool.Name)
		}
	})
	res, err := parseClaudeStream(newBR(string(raw)), sink) // includes plain-string message lines
	if err != nil {
		t.Fatalf("golden stream: %v", err)
	}
	const answer = "Hi! Here's what `ls` shows in `/home/user/project`."
	assertGoldenClaudeResult(t, res, text.String(), answer, tools)
	if deltas := goldenStreamDeltas(t, raw); deltas != answer {
		t.Errorf("stream_event text deltas = %q", deltas)
	}
}

func assertGoldenClaudeResult(t *testing.T, res wire.Result, text, answer string, tools []string) {
	t.Helper()
	if text != answer || res.Answer != answer {
		t.Errorf("text = %q, answer = %q", text, res.Answer)
	}
	if len(tools) != 1 || tools[0] != "Bash" {
		t.Errorf("tool events = %q, want one Bash", tools)
	}
	if res.SessionID != "uuid_01" || res.Model != "claude-opus-5-5" || res.Usage.CostUSD != 0.359989 || res.Usage.OutputTokens != 470 {
		t.Errorf("res = %+v", res)
	}
}

func goldenStreamDeltas(t *testing.T, raw []byte) string {
	t.Helper()
	var deltas strings.Builder
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		var inner struct {
			Type  string          `json:"type"`
			Event json.RawMessage `json:"event"`
		}
		if err := json.Unmarshal([]byte(line), &inner); err != nil || inner.Type != "stream_event" {
			continue
		}
		var se anthropic.MessageStreamEventUnion
		if err := json.Unmarshal(inner.Event, &se); err != nil {
			t.Fatal(err)
		}
		if d, ok := se.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			if td, ok := d.Delta.AsAny().(anthropic.TextDelta); ok {
				deltas.WriteString(td.Text)
			}
		}
	}
	return deltas.String()
}

func writeClaudeJSON(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if body == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeTier(t *testing.T) {
	cases := []struct{ name, json, plan, want string }{
		{"user 20x", `{"oauthAccount":{"userRateLimitTier":"default_claude_max_20x"}}`, "max", "Ultra"},
		{"org 5x", `{"oauthAccount":{"organizationRateLimitTier":"default_claude_max_5x"}}`, "max", "Max 5x"},
		{"user beats org", `{"oauthAccount":{"userRateLimitTier":"default_claude_max_5x","organizationRateLimitTier":"default_claude_max_20x"}}`, "max", "Max 5x"},
		{"no tier, Max plan", `{"oauthAccount":{"userRateLimitTier":"default"}}`, "max", "Max"},
		{"no tier, Pro plan in parens", `{}`, " (pro) ", "Pro"},
		{"no file", ``, "pro", "Pro"},
		{"malformed file", `{`, "pro", "Pro"},
		{"nothing known", ``, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeClaudeJSON(t, c.json)
			if got := claudeAgent.Plan(context.Background(), providers.AuthState{Present: true, PlanHint: strings.Trim(c.plan, " ()")}).Name; got != c.want {
				t.Errorf("claudeTier(%q) = %q, want %q", c.plan, got, c.want)
			}
		})
	}
}

func TestClaudeLoginProbeLoggedIn(t *testing.T) {
	resetClaudeProbe()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	pathstest.FakeVendor(t, "claude", `[ "$#" -eq 2 ] && [ "$1" = auth ] && [ "$2" = status ] || exit 1
echo '{"loggedIn":true,"subscriptionType":"max"}'`)
	loggedIn, plan, _ := claudeLoginProbe()
	if !loggedIn || plan != " (max)" {
		t.Errorf("claudeLoginProbe() = %v, %q; want true, \" (max)\"", loggedIn, plan)
	}
}

func TestClaudeLoginProbeScrubsParentEnv(t *testing.T) {
	resetClaudeProbe()
	t.Cleanup(resetClaudeProbe)
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HARNESS_KEEP_ENV", "")
	t.Setenv(providers.EnvSessionProfile, "parent-route")
	t.Setenv("PROBE_CONTROL", "preserved")
	envFile := filepath.Join(dir, "probe-env")
	t.Setenv("PROBE_ENV_FILE", envFile)
	pathstest.FakeVendor(t, "claude", `printf 'CLAUDE_PROFILE=%s\nPROBE_CONTROL=%s\n' "${CLAUDE_PROFILE-unset}" "$PROBE_CONTROL" > "$PROBE_ENV_FILE"
echo '{"loggedIn":true,"subscriptionType":"max"}'`)
	loggedIn, plan, err := claudeLoginProbe()
	if err != nil || !loggedIn || plan != " (max)" {
		t.Fatalf("claudeLoginProbe() = %v, %q, %v", loggedIn, plan, err)
	}
	got, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := "CLAUDE_PROFILE=unset\nPROBE_CONTROL=preserved\n"; string(got) != want {
		t.Fatalf("probe env = %q, want %q", got, want)
	}
}

// The probe is tri-state: a failed probe is an error (unknown), never "logged out".
func TestClaudeLoginProbeTriState(t *testing.T) {
	for _, c := range []struct {
		name, body string
		noBin      bool
		wantIn     bool
		wantPlan   string
		wantErr    bool
	}{
		{"logged in", `echo '{"loggedIn":true,"subscriptionType":"max"}'`, false, true, " (max)", false},
		{"logged out json on nonzero exit", `echo '{"loggedIn":false}'; exit 1`, false, false, "", false},
		{"bad json", `echo 'not json'`, false, false, "", true},
		{"nonzero exit no output", `exit 3`, false, false, "", true},
		{"missing binary", "", true, false, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetClaudeProbe()
			t.Cleanup(resetClaudeProbe)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("PATH", t.TempDir())
			if !c.noBin {
				pathstest.FakeVendor(t, "claude", c.body)
			}
			in, plan, err := claudeLoginProbe()
			if in != c.wantIn || plan != c.wantPlan || (err != nil) != c.wantErr {
				t.Errorf("probe = %v %q %v; want %v %q err=%v", in, plan, err, c.wantIn, c.wantPlan, c.wantErr)
			}
		})
	}
}

func TestClaudeLoginProbeNativeInstall(t *testing.T) {
	resetClaudeProbe()
	t.Cleanup(resetClaudeProbe)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	versions := filepath.Join(os.Getenv("HOME"), ".local", "share", "claude", "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" + `[ "$#" -eq 2 ] && [ "$1" = auth ] && [ "$2" = status ] || exit 1
 echo '{"loggedIn":true,"subscriptionType":"max"}'
`
	if err := os.WriteFile(filepath.Join(versions, "2.0.14"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := claudeAgent.Available(); err != nil {
		t.Fatalf("native install unavailable: %v", err)
	}
	loggedIn, plan, err := claudeLoginProbe()
	if err != nil || !loggedIn || plan != " (max)" {
		t.Fatalf("native auth probe = %v, %q, %v; want true, max, nil", loggedIn, plan, err)
	}
}

// `claude auth status` keeps claude's own login but no registry credential and no other agent's login.
func TestClaudeLoginProbeScrubsCredentials(t *testing.T) {
	resetClaudeProbe()
	t.Cleanup(resetClaudeProbe)
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks",`+
		`"host":"https://dev.cloud.databricks.com","auth":{"type":"key","env":"DEV_WS_TOKEN"}}}}}`)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for _, k := range []string{"DEV_WS_TOKEN", "OPENCODE_API_KEY", "GH_TOKEN", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		t.Setenv(k, "SENTINEL")
	}
	dump := filepath.Join(t.TempDir(), "env")
	pathstest.FakeVendor(t, "claude", "/usr/bin/env > "+dump+"\necho '{\"loggedIn\":true,\"subscriptionType\":\"max\"}'")
	if in, plan, err := claudeLoginProbe(); !in || plan != " (max)" || err != nil {
		t.Fatalf("probe = %v %q %v; want logged in (max)", in, plan, err)
	}
	env, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"DEV_WS_TOKEN", "OPENCODE_API_KEY", "GH_TOKEN", "OPENAI_API_KEY"} {
		if strings.Contains("\n"+string(env), "\n"+k+"=") {
			t.Errorf("claude auth status inherited %s", k)
		}
	}
	if !strings.Contains(string(env), "CLAUDE_CODE_OAUTH_TOKEN=SENTINEL") {
		t.Error("claude auth status lost its own login CLAUDE_CODE_OAUTH_TOKEN")
	}
}

// resetClaudeProbe drops the memo so the next claudeLoginProbe runs the binary again.
func resetClaudeProbe() {
	claudeProbeMemo.Drop()
}

// fakeLiveClaude is a shell claude that holds stdin open and answers each stream-json user turn with a result.
const fakeLiveClaude = `#!/bin/sh
mem=none
while IFS= read -r line; do
  p=$(printf '%s' "$line" | sed -n 's/.*"content":"\([^"]*\)".*/\1/p')
  case "$p" in
    remember*) mem=${p#remember }; ans=ok ;;
    slow*) : > "$CLAUDE_CONFIG_DIR/slow-started"; while [ ! -e "$CLAUDE_CONFIG_DIR/slow-go" ]; do sleep 0.02; done; ans=slow ;;
    boom*) printf '{"type":"error","message":"kaboom"}\n'; ans=late ;;
    *) ans="$mem pid $$" ;;
  esac
  printf '{"type":"assistant","session_id":"s1","message":{"content":[{"type":"text","text":"%s"}]}}\n' "$ans"
  printf '{"type":"result","subtype":"success","is_error":false,"result":"%s","session_id":"s1","usage":{"input_tokens":1,"output_tokens":1}}\n' "$ans"
done
`

// liveEnv gives the test a short /tmp config dir (a unix socket path caps at 104 bytes on macOS, 108 on Linux) holding the fake claude.
func liveEnv(t *testing.T) { liveEnvScript(t, fakeLiveClaude) }

func liveEnvScript(t *testing.T, script string) {
	t.Helper()
	pathstest.ShortConfigDir(t)
	if err := os.WriteFile(filepath.Join(paths.BinDir(), "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// startHolder claims a live record and runs ServeLive on it; the returned func waits for the holder and its error.
func startHolder(t *testing.T, ctx context.Context, idle time.Duration) (string, func() error) {
	t.Helper()
	return agenttest.StartHolder(t, ctx, "claude", idle, testLauncher{})
}

type collect struct {
	mu  sync.Mutex
	got []wire.Event
}

func (c *collect) Emit(ev wire.Event) error {
	c.mu.Lock()
	c.got = append(c.got, ev)
	c.mu.Unlock()
	return nil
}

// testLauncher is the found claude with the caller's args, skipping the real launch plan.
type testLauncher struct{}

func (testLauncher) Claude(_ context.Context, o agent.ClaudeOptions, args []string) (agent.ClaudeLaunch, error) {
	bin, err := ClaudeBinary()
	if err != nil {
		return agent.ClaudeLaunch{}, err
	}
	return agent.ClaudeLaunch{Binary: bin, Argv: args, Env: os.Environ(), Set: map[string]string{providers.EnvSessionProfile: o.Provider}}, nil
}

// Two turns recall each other on one claude process, a concurrent send is refused, and cancel ends the session.
func TestLiveSessionTurnsRefusalAndCancel(t *testing.T) {
	liveEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	id, wait := startHolder(t, ctx, time.Minute)
	if info, err := os.Stat(agent.LiveSocket(id)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket = %v, %v; want mode 0600", info, err)
	}
	assertLiveRecall(t, id)
	slow := make(chan error, 1)
	go func() { _, e := agent.SendLive(context.Background(), id, "slow", wire.Discard); slow <- e }()
	started := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "slow-started")
	for range 400 {
		if _, err := os.Stat(started); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := agent.SendLive(context.Background(), id, "second", wire.Discard); err != agent.ErrLiveBusy {
		t.Errorf("concurrent send = %v, want ErrLiveBusy", err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "slow-go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-slow; err != nil {
		t.Errorf("the in-flight turn must finish: %v", err)
	}
	cancel()
	assertLiveCancelled(t, id, wait)
}

func assertLiveRecall(t *testing.T, id string) {
	t.Helper()
	var sink collect
	env, err := agent.SendLive(context.Background(), id, "remember 42", &sink)
	if err != nil || env.Status != wire.StatusOK || env.Answer != "ok" {
		t.Fatalf("turn 1 = %+v, %v", env, err)
	}
	if first := sink.got[0].Type; first != wire.EventStart {
		t.Errorf("first event = %s, want start", first)
	}
	env2, err := agent.SendLive(context.Background(), id, "what number", wire.Discard)
	if err != nil || !strings.HasPrefix(env2.Answer, "42 pid ") {
		t.Fatalf("turn 2 = %+v, %v (the second turn must recall the first)", env2, err)
	}
}

func assertLiveCancelled(t *testing.T, id string, wait func() error) {
	t.Helper()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(agent.LiveSocket(id)); !os.IsNotExist(err) {
		t.Errorf("socket must be removed on exit: %v", err)
	}
	got, _ := agent.LoadRecord(id)
	if got.Status != "error" || got.Error != "cancelled" {
		t.Errorf("record = %s %q, want error cancelled", got.Status, got.Error)
	}
}

// An idle session ends as ok after the short override; a send after that is refused as ended.
func TestLiveIdleTimeoutEndsOK(t *testing.T) {
	liveEnv(t)
	id, wait := startHolder(t, context.Background(), 300*time.Millisecond)
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if got, _ := agent.LoadRecord(id); got.Status != "ok" {
		t.Errorf("idle end status = %s, want ok", got.Status)
	}
	if _, err := agent.SendLive(context.Background(), id, "x", wire.Discard); err == nil || !strings.Contains(err.Error(), "ended") {
		t.Errorf("send after idle = %v, want an ended error", err)
	}
}

// fakeNoisyClaude writes 200 KB to stdout before it reads stdin, so an undrained stdout blocks claude and,
// with a large prompt, the holder's stdin write too.
const fakeNoisyClaude = `#!/bin/sh
printf '{"type":"system","subtype":"noise","message":"%s"}\n' "$(head -c 200000 /dev/zero | tr '\0' x)"
` + fakeLiveClaude

// A large prompt sent while claude's between-turn output fills the stdout pipe must not deadlock the holder.
func TestLiveLargePromptWithPendingOutputDoesNotDeadlock(t *testing.T) {
	liveEnvScript(t, fakeNoisyClaude)
	id, wait := startHolder(t, context.Background(), 300*time.Millisecond)
	done := make(chan error, 1)
	go func() {
		_, err := agent.SendLive(context.Background(), id, strings.Repeat("y", 200000), wire.Discard)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("send deadlocked: stdin write blocked behind an undrained stdout")
	}
	if err := wait(); err != nil { // the idle timer must still run and end the session
		t.Fatal(err)
	}
}

// A turn that fails with a top-level error must never leave its late result to answer the next send.
func TestLiveTurnErrorNeverLeaksIntoNextTurn(t *testing.T) {
	liveEnv(t)
	id, wait := startHolder(t, context.Background(), 5*time.Second)
	if _, err := agent.SendLive(context.Background(), id, "boom", wire.Discard); err != nil {
		t.Logf("failing turn: %v", err)
	}
	env, err := agent.SendLive(context.Background(), id, "remember 7", wire.Discard)
	if err == nil && env.Answer == "late" {
		t.Fatalf("next send got the previous turn's answer %q", env.Answer)
	}
	_ = wait()
}

// fakeCostClaude answers each turn with a result whose total_cost_usd accumulates 0.5 per turn, as claude's does.
const fakeCostClaude = `#!/bin/sh
n=0
while IFS= read -r line; do
  n=$((n + 1))
  printf '{"type":"result","subtype":"success","is_error":false,"result":"r","session_id":"s1","total_cost_usd":%s,"usage":{"input_tokens":1,"output_tokens":1}}\n' "$(echo "$n" | awk '{print $1 * 0.5}')"
done
`

// Claude's total_cost_usd accumulates over the process; each live turn must report only its own cost.
func TestLiveClaudeCostIsPerTurn(t *testing.T) {
	liveEnvScript(t, fakeCostClaude)
	ctx, cancel := context.WithCancel(context.Background())
	id, wait := startHolder(t, ctx, time.Minute)
	defer func() { cancel(); _ = wait() }()
	for turn := 1; turn <= 3; turn++ {
		env, err := agent.SendLive(context.Background(), id, "x", wire.Discard)
		if err != nil || env.Usage.CostUSD != 0.5 {
			t.Fatalf("turn %d = cost %v, %v; want the turn's own 0.5", turn, env.Usage.CostUSD, err)
		}
	}
}

// fakeExitAfterResult answers the first turn with a bulky assistant line and the final result, then exits at once.
const fakeExitAfterResult = `#!/bin/sh
IFS= read -r line
pad=$(head -c 400000 /dev/zero | tr '\0' x)
printf '{"type":"assistant","session_id":"s1","message":{"content":[{"type":"text","text":"%s"}]}}\n' "$pad"
printf '{"type":"result","subtype":"success","is_error":false,"result":"final","session_id":"s1","usage":{"input_tokens":1,"output_tokens":1}}\n'
`

// The last turn's tail survives a child that exits right after writing it: Wait runs only once stdout is drained.
func TestLiveFinalResultSurvivesImmediateChildExit(t *testing.T) {
	for i := range 30 {
		liveEnvScript(t, fakeExitAfterResult)
		id, wait := startHolder(t, context.Background(), time.Minute)
		env, err := agent.SendLive(context.Background(), id, "go", wire.Discard)
		if err != nil || env.Answer != "final" {
			t.Fatalf("run %d: answer %q, err %v; want the final result delivered", i, env.Answer, err)
		}
		_ = wait()
	}
}

// A cancelled run still reaps the child and ends the holder: serveChild's Wait goroutine must not block forever.
func TestLiveCancelEndsHolderAndReapsChild(t *testing.T) {
	liveEnvScript(t, "#!/bin/sh\nexec /bin/sleep 20\n")
	ctx, cancel := context.WithCancel(context.Background())
	_, wait := startHolder(t, ctx, time.Minute)
	cancel()
	done := make(chan error, 1)
	go func() { done <- wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("holder did not end after cancel: serveChild deadlocked on pump.done")
	}
}

// syncBuffer is a bytes.Buffer safe for Tail writing while the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type tailOutcome struct {
	rec agent.Record
	err error
}

type resultNoticeWriter struct {
	out     syncBuffer
	results chan struct{}
}

func (w *resultNoticeWriter) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	if strings.Contains(string(p), `"type":"result"`) {
		w.results <- struct{}{}
	}
	return n, err
}

func awaitTailResult(t *testing.T, results <-chan struct{}, done <-chan tailOutcome) {
	t.Helper()
	select {
	case <-results:
	case got := <-done:
		t.Fatalf("tail ended before the next turn result: status=%s err=%v", got.rec.Status, got.err)
	case <-time.After(3 * time.Second):
		t.Fatal("tail did not copy the turn result")
	}
}

// Runs at agent's production tail cadence (250ms poll): the swap seam is unexported in package agent.
func TestTailEventsFollowsTwoLiveTurns(t *testing.T) {
	liveEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	id, wait := startHolder(t, ctx, time.Minute)
	t.Cleanup(func() { cancel(); _ = wait() })
	rec, err := agent.LoadRecord(id)
	if err != nil {
		t.Fatal(err)
	}
	sendTailTurn(t, id, "remember 42", "ok")
	out := &resultNoticeWriter{results: make(chan struct{}, 4)}
	tailCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	done := make(chan tailOutcome, 1)
	go func() {
		got, err := agent.TailEvents(tailCtx, rec, true, out)
		done <- tailOutcome{got, err}
	}()
	awaitTailResult(t, out.results, done)
	sendTailTurn(t, id, "what number", "42 pid ")
	awaitTailResult(t, out.results, done)
	select {
	case got := <-done:
		t.Fatalf("healthy live session lost after a turn: status=%s err=%v", got.rec.Status, got.err)
	case <-time.After(750 * time.Millisecond): // three production tail polls
	}
	stop()
	got := <-done
	if !errors.Is(got.err, context.Canceled) || !got.rec.Alive() {
		t.Fatalf("tail cancellation = %s, %v; want a still-live record and cancellation", got.rec.Status, got.err)
	}
	if n := strings.Count(out.out.String(), `"type":"result"`); n != 2 {
		t.Fatalf("copied %d turn results, want 2", n)
	}
}

func sendTailTurn(t *testing.T, id, prompt, answer string) {
	t.Helper()
	env, err := agent.SendLive(context.Background(), id, prompt, wire.Discard)
	if err != nil || env.Status != wire.StatusOK || !strings.HasPrefix(env.Answer, answer) {
		t.Fatalf("live turn = %+v, %v; want answer starting %q", env, err, answer)
	}
}

func TestPermissionNoToolsProcessIsolation(t *testing.T) {
	hermetic(t, dbxProfiles)
	for _, profile := range []string{"ultra", "qa"} {
		_, args, _ := launchOf(t, agent.RunOptions{NoTools: true, Provider: profile, SessionID: "resume-id"})
		for flag, want := range map[string]string{"--tools": "", "--mcp-config": `{"mcpServers":{}}`, "--resume": "resume-id"} {
			at := slices.Index(args, flag)
			if at < 0 || at+1 >= len(args) || args[at+1] != want {
				t.Fatalf("%s: missing isolation %s=%q in %q", profile, flag, want, args)
			}
		}
		if !slices.Contains(args, "--strict-mcp-config") {
			t.Fatalf("%s: ambient MCP configs remain enabled: %q", profile, args)
		}
	}
	if slices.Contains(toolArgs(agent.RunOptions{}), "--strict-mcp-config") {
		t.Fatal("no-flag default changed")
	}
}
