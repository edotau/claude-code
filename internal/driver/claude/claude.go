package claude

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/remote/egress"
	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// claudeEngine execs `claude -p --output-format stream-json` with the launcher's provider env and overlay.
type claudeEngine struct{ agent.BaseEngine }

// claudeAgent is the claude agent; Name and Caps come from the shared base, like every other agent.
var claudeAgent = claudeEngine{agent.NewBaseEngine("claude").WithRouteCaps(agent.RouteCaps{Login: "claude"})}

var _ agent.Engine = claudeAgent

// Available also finds the native installer's versions dir (ClaudeBinary), which the base's AgentBin lookup does not.
func (claudeEngine) Available() error { _, err := ClaudeBinary(); return err }

func (claudeEngine) Run(ctx context.Context, req agent.RunOptions, sink wire.Sink) (wire.Result, error) {
	if err := refuseCapabilities(req); err != nil {
		return wire.Result{}, err
	}
	if req.NoTools {
		if res, done, err := runNoTools(ctx, req, sink); done {
			return res, err
		}
	}
	prompt, err := claudeAttach(req.Prompt, req.Attach, req.Tools)
	if err != nil {
		return wire.Result{}, err
	}
	return claudeExec(ctx, req, prompt, sink)
}

// runNoTools takes the HTTP one-shot; done is false when the request needs the claude process instead.
func runNoTools(ctx context.Context, req agent.RunOptions, sink wire.Sink) (wire.Result, bool, error) {
	// the one-shot has no system-prompt or schema channel and a session must resume through the process
	if req.BudgetUSD != nil || req.SessionID != "" || req.SystemPrompt != "" || req.AppendSystemPrompt != "" || req.OutputSchema != "" || req.Persona != nil {
		return wire.Result{}, false, nil
	}
	return claudeOneShot(ctx, req, sink)
}

// claudeExec runs one stream-json claude process to completion and parses its events into sink.
func claudeExec(ctx context.Context, req agent.RunOptions, prompt string, sink wire.Sink) (wire.Result, error) {
	args := claudeStreamArgs(req, false)
	cmd, tail, cl, err := claudeCmd(ctx, req, args, prompt)
	if err != nil {
		return wire.Result{}, err
	}
	wd := newIdleWatch(claudeIdleLimit, nil) // RunEventStream binds the kill once the child starts
	wd.Follow(ctx)
	wd.row = cl.Set[providers.EnvSessionProfile]
	wd.onModel = func(m string) { agent.NotePick(ctx, m) } // an unchanged model writes nothing
	parse := func(br *bufio.Reader, sink wire.Sink, spoke func(), logs ...wire.StreamLog) (wire.Result, error) {
		return parseClaudeEvents(br, false, wd, sink, spoke, logs...)
	}
	res, err := agent.RunEventStream(ctx, "claude", cmd, tail, sink, parse, agent.StreamOpts{Idle: wd.IdleTimeout})
	res.Provider = cl.Set[providers.EnvSessionProfile]
	return res, wd.stalledErr(err)
}

func claudeStreamArgs(req agent.RunOptions, live bool) []string {
	argv := []string{"--print"}
	if live {
		argv = append(argv, "--input-format", "stream-json")
	}
	argv = append(argv, "--output-format", "stream-json", "--verbose", "--include-partial-messages")
	return claudeArgs(argv, req)
}

// withoutDraftKey drops a top-level "$schema" from s: claude 2.1.332 cannot resolve the 2020-12 meta-schema id.
func withoutDraftKey(s string) string {
	var doc map[string]json.RawMessage
	if json.Unmarshal([]byte(s), &doc) != nil || doc["$schema"] == nil {
		return s
	}
	delete(doc, "$schema")
	b, err := json.Marshal(doc)
	if err != nil {
		return s
	}
	return string(b)
}

// claudeArgs appends the request's option flags to base; effort is RenderSettings', tools toolArgs'.
func claudeArgs(args []string, req agent.RunOptions) []string {
	if req.BudgetUSD != nil {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(*req.BudgetUSD, 'g', -1, 64))
	}
	for _, f := range []struct {
		on   bool
		flag []string
	}{
		{req.NoPersist, []string{"--no-session-persistence"}},
		{req.DebugFile != "", []string{"--debug-file", req.DebugFile}},
		{req.SystemPrompt != "", []string{"--system-prompt", req.SystemPrompt}},
		{req.AppendSystemPrompt != "", []string{"--append-system-prompt", req.AppendSystemPrompt}},
		{req.OutputSchema != "", []string{"--json-schema", withoutDraftKey(req.OutputSchema)}},
		{req.SessionID != "", []string{"--resume", req.SessionID}},
		{req.Tools, []string{"--permission-mode", "bypassPermissions"}}, // a --tools leaf holds its tools whole
	} {
		if f.on {
			args = append(args, f.flag...)
		}
	}
	if req.Persona != nil {
		args = append(args, agent.PersonaArgs(req.Persona)...)
	}
	return args
}

// toolArgs is the tool-set tail, last after the rendered settings as it was after the permission flags.
func toolArgs(req agent.RunOptions) []string {
	if req.NoTools {
		return []string{"--tools", "", "--setting-sources", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`}
	}
	if !req.Tools {
		return []string{"--tools", "", "--setting-sources", ""}
	}
	return []string{"--exclude-dynamic-system-prompt-sections"} // per-session sections defeat the prompt cache
}

// RenderSettings adds --effort after the base launch env/args.
func (e claudeEngine) RenderSettings(ctx context.Context, s agent.Settings, env, argv []string) ([]string, []string, []string, func(), error) {
	env, argv, emulated, cleanup, err := e.BaseEngine.RenderSettings(ctx, s, env, argv)
	if err != nil {
		return env, argv, emulated, cleanup, err
	}
	effort, err := claudeEffort(s.Effort)
	if err != nil {
		return env, argv, emulated, cleanup, err
	}
	if effort != "" {
		argv = append(argv, "--effort", effort)
	}
	return env, argv, emulated, cleanup, nil
}

// claudeEffort spells e for claude's CLI and Messages API; "" stays "" and an unknown effort is refused.
func claudeEffort(e string) (string, error) {
	if err := agentid.ValidEffort(agentid.Claude, e); err != nil {
		return "", err
	}
	native, _ := agentid.NativeEffort(agentid.Claude, agentid.Effort(e))
	return native, nil
}

// launchProvider is the profile claudeCmd launches req on: the cell's row, the request's, else the session profile.
func launchProvider(ctx context.Context, req agent.RunOptions) string {
	if p := cmp.Or(req.CellRoute.Profile, req.Provider); p != "" {
		return p
	}
	if req.Values != nil {
		ctx = proc.WithValues(ctx, req.Values)
	}
	return agent.SessionProviderContext(ctx, agentid.Claude)
}

// launchTarget is req's launch profile row; Provider nil when it does not resolve (never Anthropic, so no auto judge).
func launchTarget(ctx context.Context, req agent.RunOptions) providers.Target {
	t := providers.Target{Model: req.Model}
	if reg, err := providers.Load(); err == nil {
		if p, err := reg.Get(launchProvider(ctx, req)); err == nil {
			t.Provider = p
		}
	}
	return t
}

// claudeCmd renders the settings onto args, resolves the launch plan and returns the unstarted child (--env last).
func claudeCmd(ctx context.Context, req agent.RunOptions, args []string, prompt string) (*exec.Cmd, *retry.TailBuffer, agent.ClaudeLaunch, error) {
	if err := refuseClaudeCmd(req); err != nil {
		return nil, nil, agent.ClaudeLaunch{}, err
	}
	origArgs := args
	p := agent.CLIPrep{Argv: args}
	if _, err := agent.PrepareLaunch(ctx, claudeAgent, req, launchTarget(ctx, req), &p); err != nil {
		return nil, nil, agent.ClaudeLaunch{}, retry.Fatal("claude", err)
	}
	args = append(p.Argv, toolArgs(req)...)
	cmArgs, req, err := codemodeArgs(ctx, req)
	if err != nil {
		return nil, nil, agent.ClaudeLaunch{}, retry.Fatal("claude", err)
	}
	args = append(args, cmArgs...)
	o := agent.ClaudeOptions{Provider: launchProvider(ctx, req), Model: req.MarkedModel(), KeepEnv: req.KeepEnv, Dispatch: true,
		ProfileAsked: cmp.Or(req.CellRoute.Profile, req.Provider) != ""}
	cl, err := req.Launcher.Claude(ctx, o, args)
	if err != nil {
		return nil, nil, cl, err
	}
	if err := agent.CheckRouteNames(proc.Values(cl.Set).Env(), req.OverlaySet, req.OverlayUnset); err != nil {
		return nil, nil, cl, err
	}
	cmd, tail, err := claudeChild(ctx, req, cl, p.Env, prompt)
	if err == nil && len(cmArgs) > 0 { // the child's own env: a shell-kept endpoint counts
		if err = checkCodemodeRoute(launchTarget(ctx, req).Provider, childBase(cl.Argv, cmd.Env)); err != nil {
			return claudeCmd(ctx, agent.CodemodeOff(ctx, agentid.Claude, err.Error(), req), origArgs, prompt)
		}
	}
	return cmd, tail, cl, err
}

// codemodeArgs is the --settings arg for a code-mode request (tools only); a route off the Anthropic API runs it off.
func codemodeArgs(ctx context.Context, req agent.RunOptions) ([]string, agent.RunOptions, error) {
	if !req.Tools {
		return nil, req, nil
	}
	args, err := codemodeSettings(req.Codemode)
	if err != nil || len(args) == 0 {
		return nil, req, err
	}
	if why := checkCodemodeRoute(launchTarget(ctx, req).Provider, ""); why != nil {
		return nil, agent.CodemodeOff(ctx, agentid.Claude, why.Error(), req), nil
	}
	return args, req, nil
}

// childBase is the endpoint the child uses: the --settings env block's ANTHROPIC_BASE_URL over the process env's.
func childBase(argv, env []string) string {
	base := proc.LastEnv(env, proc.EnvAnthropicBaseURL)
	for i, a := range argv {
		v, ok := strings.CutPrefix(a, "--settings=")
		if a == "--settings" && i+1 < len(argv) {
			v, ok = argv[i+1], true
		}
		if !ok {
			continue
		}
		doc, err := readSettings(v)
		if m, isMap := doc["env"].(map[string]any); err == nil && isMap {
			if u, isStr := m[proc.EnvAnthropicBaseURL].(string); isStr {
				base = u
			}
		}
	}
	return base
}

// refuseClaudeCmd is claudeCmd's refusal before any resolution: an unsupported capability or no launcher.
func refuseClaudeCmd(req agent.RunOptions) error {
	if err := refuseCapabilities(req); err != nil {
		return err
	}
	if req.Launcher == nil {
		return errors.New("claude: no launcher (caller must set RunOptions.Launcher)")
	}
	return nil
}

// claudeChild is the unstarted child for the resolved launch, its --env applied last.
func claudeChild(ctx context.Context, req agent.RunOptions, cl agent.ClaudeLaunch, launchEnv []string, prompt string) (*exec.Cmd, *retry.TailBuffer, error) {
	cmd, tail := agent.ChildCmd(ctx, cl.Binary, cl.Argv, nil, req.WorkDir, prompt, req.KeepEnv, false)
	keep := append(slices.Clone(req.KeepEnv), slices.Collect(maps.Keys(cl.Set))...)
	set := agent.RouteEnvLast(dropLaunchOwned(launchEnv), proc.Values(cl.Set).Env())
	cmd.Env = agent.EnvSpec{Mode: agent.EnvPrescrubbed, Base: cl.Env, Keep: keep, Set: set, Extra: req.Env}.Environ(ctx) // --env last, so it wins
	return cmd, tail, nil
}

// readSettings is a --settings value: inline JSON when it starts with "{", else a file path.
func readSettings(v string) (map[string]any, error) {
	raw := []byte(v)
	if !strings.HasPrefix(strings.TrimSpace(v), "{") {
		var err error
		if raw, err = os.ReadFile(v); err != nil {
			return nil, err
		}
	}
	doc := map[string]any{}
	return doc, json.Unmarshal(raw, &doc)
}

type claudeEvent struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	SessionID       string          `json:"session_id"`
	Model           string          `json:"model"`
	ParentToolUseID string          `json:"parent_tool_use_id"`
	Message         json.RawMessage `json:"message"`
	IsError         bool            `json:"is_error"`
	Attempt         int             `json:"attempt"`
	MaxRetries      int             `json:"max_retries"`
	RetryDelayMs    int             `json:"retry_delay_ms"`
	ErrorStatus     *int            `json:"error_status"`
	Result          string          `json:"result"`
	Errors          []string        `json:"errors"`
	TotalCostUSD    float64         `json:"total_cost_usd"`
	NumTurns        int             `json:"num_turns"`
	Structured      json.RawMessage `json:"structured_output"`

	Event streamEventLean `json:"event"`

	Usage struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// streamEventLean is the few stream_event fields usage needs; a mistyped field is skipped, as the SDK union did.
type streamEventLean struct {
	Type    string `json:"type"`
	Message struct {
		ID    string      `json:"id"`
		Usage usageTokens `json:"usage"`
	} `json:"message"`
	Usage usageTokens `json:"usage"`
}

func (e *streamEventLean) UnmarshalJSON(b []byte) error {
	type plain streamEventLean
	_ = json.Unmarshal(b, (*plain)(e))
	return nil
}

type usageTokens struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func (u usageTokens) wire() wire.Usage {
	return wire.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheReadInputTokens, CacheCreationTokens: u.CacheCreationInputTokens}
}

type claudeBlock struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Text      string `json:"text"`
	ToolUseID string `json:"tool_use_id"`
}

type claudeMessage struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Content []claudeBlock `json:"content"`
	Usage   usageTokens   `json:"usage"`
}

func foldDeltaUsage(res *wire.Usage, u usageTokens) {
	if u.InputTokens > 0 {
		res.InputTokens = u.InputTokens
	}
	if u.CacheCreationInputTokens > 0 {
		res.CacheCreationTokens = u.CacheCreationInputTokens
	}
	if u.CacheReadInputTokens > 0 {
		res.CacheReadTokens = u.CacheReadInputTokens
	}
	res.OutputTokens = u.OutputTokens
}

// decodeMessage decodes a raw message (string or absent = zero); a var so tests count calls.
var decodeMessage = func(raw json.RawMessage) claudeMessage {
	var m claudeMessage
	if len(raw) == 0 || raw[0] != '{' {
		return m
	}
	_ = json.Unmarshal(raw, &m) // best-effort: an odd shape must never fail the stream
	return m
}

// topMessage decodes a top-level assistant or user message, the only ones the stream state and idle watch read.
func (ev claudeEvent) topMessage() claudeMessage {
	if ev.ParentToolUseID != "" || (ev.Type != "assistant" && ev.Type != "user") {
		return claudeMessage{}
	}
	return decodeMessage(ev.Message)
}

// text returns ev.Message when the event carries it as a plain string instead of an object.
func (ev claudeEvent) text() string {
	if len(ev.Message) == 0 || ev.Message[0] != '"' {
		return ""
	}
	var s string
	_ = json.Unmarshal(ev.Message, &s)
	return s
}

// isModelOutput reports a top-level assistant, stream_event or result event: the first sign the model spoke.
func (ev claudeEvent) isModelOutput() bool {
	return ev.ParentToolUseID == "" && (ev.Type == "assistant" || ev.Type == "stream_event" || ev.Type == "result")
}

// parseClaudeEvents stops at the first result if untilResult (lines stay buffered); spoke (nil ok) fires once.
func parseClaudeEvents(br *bufio.Reader, untilResult bool, wd *idleWatch, sink wire.Sink, spoke func(),
	logs ...wire.StreamLog) (res wire.Result, _ error) {
	lg := wire.PickLog(logs)
	sawResult, spoken := false, spoke == nil
	usage := claudeUsage{messages: map[string]wire.Usage{}}
	seen := wire.Answer{Join: "\n\n"} // top-level assistant text, the answer when no result event delivers one
	partial := func(err error) (wire.Result, error) {
		if res.Answer == "" {
			res.Answer = seen.String()
		}
		return res, err
	}
	edits, tools := fileEdits{}, toolTimer{}
	tr, err := wire.DecodeEvents(br, lg, "decode stream-json", false, nil, func(ev claudeEvent, ts *wire.TransportTracker) (bool, error) {
		edits.observe(ev, &res)
		msg := ev.topMessage()
		if !sawResult {
			res.Usage = usage.observe(ev, msg)
		}
		wd.observe(ev, msg)
		if !spoken && ev.isModelOutput() {
			spoken = true
			spoke()
		}
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		if ev.Type == "result" {
			sawResult = true
		}
		state := claudeStreamState{res: &res, transport: ts, log: lg, sink: sink, seen: &seen, tools: tools}
		return state.apply(ev, msg, untilResult)
	})
	res.Transport = tr
	if err != nil {
		return partial(err)
	}
	if !sawResult {
		return partial(errNoResult)
	}
	return partial(nil) // an empty closing result falls back to the streamed text
}

type claudeStreamState struct {
	res       *wire.Result
	transport *wire.TransportTracker
	log       wire.StreamLog
	sink      wire.Sink
	seen      *wire.Answer
	tools     toolTimer
}

// toolTimer times top-level calls by tool_use id, tool_use to result (no start event, so waits count).
type toolTimer map[string]pendingTool

type pendingTool struct {
	name  string
	start time.Time
}

// started notes every tool_use of a top-level assistant message, on the adapter's own clock.
func (t toolTimer) started(ev claudeEvent, msg claudeMessage) {
	if ev.ParentToolUseID != "" {
		return
	}
	for _, b := range msg.Content {
		if b.Type == "tool_use" {
			t[b.ID] = pendingTool{name: b.Name, start: wire.Clock()}
		}
	}
}

// finished emits a tool_result event per answered tool_use of a user message; the error is the is_error field.
func (t toolTimer) finished(ev claudeEvent, sink wire.Sink) error {
	if ev.ParentToolUseID != "" || len(t) == 0 || !bytes.Contains(ev.Message, []byte(`"tool_result"`)) {
		return nil
	}
	var m struct {
		Content []editBlock `json:"content"`
	}
	if json.Unmarshal(ev.Message, &m) != nil {
		return nil
	}
	for _, b := range m.Content {
		p, ok := t[b.ToolUseID]
		if b.Type != "tool_result" || !ok {
			continue
		}
		delete(t, b.ToolUseID)
		status := "completed"
		if b.IsError {
			status = "error"
		}
		if err := sink.Emit(wire.Event{Type: wire.EventToolResult, Tool: &wire.ToolRef{Name: p.name, ID: b.ToolUseID},
			Status: status, DurationMs: wire.Clock().Sub(p.start).Milliseconds()}); err != nil {
			return err
		}
	}
	return nil
}

type claudeUsage struct {
	messages map[string]wire.Usage
	current  string
	total    wire.Usage
}

func (c *claudeUsage) observe(ev claudeEvent, msg claudeMessage) wire.Usage {
	if ev.ParentToolUseID != "" {
		return c.total
	}
	id, u := msg.ID, msg.Usage.wire()
	if ev.Type == "stream_event" {
		switch ev.Event.Type {
		case "message_start":
			id, u = ev.Event.Message.ID, ev.Event.Message.Usage.wire()
			c.current = id
		case "message_delta":
			id, u = c.current, c.messages[c.current]
			foldDeltaUsage(&u, ev.Event.Usage)
		}
	}
	if id == "" {
		return c.total
	}
	old := c.messages[id]
	// Assistant snapshots repeat stream usage and can lag behind the final message_delta.
	u = wire.Usage{InputTokens: max(u.InputTokens, old.InputTokens), OutputTokens: max(u.OutputTokens, old.OutputTokens),
		CacheReadTokens: max(u.CacheReadTokens, old.CacheReadTokens), CacheCreationTokens: max(u.CacheCreationTokens, old.CacheCreationTokens)}
	c.total.InputTokens += u.InputTokens - old.InputTokens
	c.total.OutputTokens += u.OutputTokens - old.OutputTokens
	c.total.CacheReadTokens += u.CacheReadTokens - old.CacheReadTokens
	c.total.CacheCreationTokens += u.CacheCreationTokens - old.CacheCreationTokens
	c.messages[id] = u
	return c.total
}

func (s claudeStreamState) apply(ev claudeEvent, msg claudeMessage, untilResult bool) (bool, error) {
	switch ev.Type {
	case "stream_event":
		s.transport.Event(wire.Clock())
	case "system":
		return false, applyClaudeSystem(ev, s.res, s.transport, s.log, s.sink)
	case "user":
		return false, s.tools.finished(ev, s.sink)
	case "assistant":
		s.tools.started(ev, msg)
		return false, applyClaudeAssistantEvent(ev, msg, s.res, s.log, s.sink, s.seen)
	case "result":
		if err := applyClaudeResult(s.res, ev); err != nil {
			_ = wire.EmitError(s.log, s.sink, err.Error())
			return false, err
		}
		s.log.Emit("result", ev.Result)
		return untilResult, s.emitResultOnly(ev)
	case "error":
		if t := ev.text(); t != "" {
			_ = wire.EmitError(s.log, s.sink, t)
			return false, fmt.Errorf("claude: %s", t)
		}
	}
	return false, nil
}

// emitResultOnly streams a result-only answer, which has no assistant text of its own, once.
func (s claudeStreamState) emitResultOnly(ev claudeEvent) error {
	if !s.seen.Empty() || ev.Result == "" || ev.IsError {
		return nil
	}
	return s.seen.Emit(s.sink, ev.Result, true)
}

// fileEdits pairs file-writing tool_use blocks with their non-error tool_results by id.
type fileEdits map[string]string

var fileEditTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true}

type editBlock struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
	Input     struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	} `json:"input"`
}

// observe records a written path in res.Written once its tool_result succeeds.
func (f fileEdits) observe(ev claudeEvent, res *wire.Result) {
	if ev.Type != "assistant" && ev.Type != "user" || !bytes.Contains(ev.Message, []byte(`"tool_`)) {
		return
	}
	var m struct {
		Content []editBlock `json:"content"`
	}
	if json.Unmarshal(ev.Message, &m) != nil {
		return
	}
	for _, b := range m.Content {
		f.block(b, res)
	}
}

func (f fileEdits) block(b editBlock, res *wire.Result) {
	switch b.Type {
	case "tool_use":
		if path := cmp.Or(b.Input.FilePath, b.Input.NotebookPath); fileEditTools[b.Name] && path != "" {
			f[b.ID] = path
		}
	case "tool_result":
		if path, ok := f[b.ToolUseID]; ok && !b.IsError {
			res.Written = append(res.Written, path)
		}
		delete(f, b.ToolUseID)
	}
}

var errNoResult = errors.New("claude: stream ended without a result event")

// applyClaudeSystem records an api_retry and the session model from a system event.
func applyClaudeSystem(ev claudeEvent, res *wire.Result, ts *wire.TransportTracker, lg wire.StreamLog, sink wire.Sink) error {
	if ev.Subtype == "api_retry" {
		ts.Retry(ev.ErrorStatus)
		lg.Emit("retry", fmt.Sprintf("attempt %d/%d status=%d retry_in=%dms",
			ev.Attempt, ev.MaxRetries, ts.LastStatus(), ev.RetryDelayMs))
		if err := sink.Emit(wire.Event{Type: wire.EventRetry, Status: strconv.Itoa(ts.LastStatus()),
			Text: fmt.Sprintf("attempt %d/%d retry_in=%dms", ev.Attempt, ev.MaxRetries, ev.RetryDelayMs)}); err != nil {
			return err
		}
	}
	if ev.Model != "" {
		res.Model = ev.Model
	}
	return nil
}

// applyClaudeAssistantEvent logs and emits a top-level assistant message; a subagent's is ignored.
func applyClaudeAssistantEvent(ev claudeEvent, msg claudeMessage, res *wire.Result, lg wire.StreamLog, sink wire.Sink, seen *wire.Answer) error {
	if ev.ParentToolUseID != "" {
		return nil
	}
	if msg.Model != "" {
		res.Model = msg.Model
	}
	logClaudeAssistant(lg, msg)
	return emitClaudeAssistant(sink, msg, seen)
}

func logClaudeAssistant(lg wire.StreamLog, msg claudeMessage) {
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			lg.Emit("assistant", b.Text)
		case "tool_use":
			lg.Emit("tool", b.Name)
		}
	}
}

// emitClaudeAssistant emits each text block through seen and each tool call to sink.
func emitClaudeAssistant(sink wire.Sink, msg claudeMessage, seen *wire.Answer) error {
	for _, b := range msg.Content {
		var err error
		switch b.Type {
		case "tool_use":
			err = sink.Emit(wire.ToolEvent(b.Name, b.ID))
		case "text":
			if b.Text != "" {
				err = seen.Emit(sink, b.Text, true)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func applyClaudeResult(res *wire.Result, ev claudeEvent) error {
	res.Turns = ev.NumTurns
	if ev.Result != "" && !ev.IsError { // an error result's text rides in the error, not over the partial answer
		res.Answer = ev.Result // a resumed session's closing empty result must not erase the last real one
	}
	u := ev.Usage
	if u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheCreationInputTokens != 0 || u.CacheReadInputTokens != 0 {
		res.Usage = wire.Usage{InputTokens: u.InputTokens, CacheCreationTokens: u.CacheCreationInputTokens,
			CacheReadTokens: u.CacheReadInputTokens, OutputTokens: u.OutputTokens}
	}
	res.Usage.CostUSD = ev.TotalCostUSD
	if len(ev.Structured) > 0 && string(ev.Structured) != "null" {
		res.Structured = ev.Structured
	}
	if ev.IsError {
		return resultFailure(ev)
	}
	return nil
}

// The result subtypes of claude's own cost and turn caps.
const (
	subtypeMaxBudget = "error_max_budget_usd"
	subtypeMaxTurns  = "error_max_turns"
)

// resultFailure is a failed result; a credential-helper failure is auth, so it reads as availability, not quality.
func resultFailure(ev claudeEvent) error {
	detail := cmp.Or(ev.Result, strings.Join(ev.Errors, "; "))
	err := wire.NewResultError(fmt.Errorf("claude %s: %s", ev.Subtype, detail))
	if ev.Subtype == subtypeMaxBudget || ev.Subtype == subtypeMaxTurns { // the agent's own cost or turn cap: never re-sent
		err = retry.WithBudget(err, retry.BudgetCost)
	}
	if strings.Contains(ev.Result, "apiKeyHelper") || strings.Contains(strings.Join(ev.Errors, "; "), "apiKeyHelper") {
		return &retry.Error{Kind: retry.KindAuth, Agent: "claude", Err: err}
	}
	return err
}

func claudeAttach(prompt string, attach []string, tools bool) (string, error) {
	images, rest := agent.SplitImages(attach)
	if len(images) > 0 && !tools {
		return "", fmt.Errorf("--%w %s: image attach needs tools for claude", agent.ErrBadAttach, images[0])
	}
	prompt, err := agent.AttachInline(prompt, rest)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, p := range images {
		sb.WriteString(assets.ClaudeAttachImage(p))
	}
	prompt += sb.String()
	return prompt, nil
}

func init() {
	agent.Register(agent.Agent{Row: row, Driver: claudeAgent, Launcher: claudeLauncher{}})
}

// row is claude's roster entry: binary, dialects, capabilities, events and launch hints.
var row = agent.Schema{Name: "claude", CLI: agent.CLI{
	PromptVia: agent.PromptStdin,
	Flags: []string{"--print", "--output-format", "--verbose", "--include-partial-messages", "--input-format",
		"--max-budget-usd", "--no-session-persistence", "--debug-file", "--system-prompt",
		"--append-system-prompt", "--json-schema", "--resume", "--permission-mode", "--tools", "--setting-sources",
		"--strict-mcp-config", "--mcp-config", "--exclude-dynamic-system-prompt-sections", "--effort",
		"--agents", "--agent", "--settings"},
	Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
		Tools: agent.Native, NoTools: agent.Native, Autonomous: agent.Native, NoPersist: agent.Native, DebugFile: agent.Native,
		SystemPrompt: agent.Native, AppendSystemPrompt: agent.Native, Env: agent.Native, OutputSchema: agent.Native, Persona: agent.Native},
	Events: []string{wire.EventStart, wire.EventText, wire.EventToolUse, wire.EventToolResult, wire.EventRetry, wire.EventResult, wire.EventError},
	Cost:   true, ConfigureHint: "run `claude-code configure agents`", LoginArgv: []string{"claude", "auth", "login"},
}}

// claudeLauncher implements Launcher over Claude: the plan's overlay is written, its env layered on the process env.
type claudeLauncher struct{}

func (claudeLauncher) Claude(ctx context.Context, o agent.ClaudeOptions, args []string) (agent.ClaudeLaunch, error) {
	plan, err := claudePlan(ctx, agent.LaunchOptions{Provider: o.Provider, Model: o.Model, KeepEnv: o.KeepEnv, Dispatch: o.Dispatch, Asked: o.ProfileAsked}, args)
	if err != nil {
		return agent.ClaudeLaunch{}, err
	}
	if err := plan.WriteFiles(); err != nil {
		return agent.ClaudeLaunch{}, err
	}
	plan.Values = runValues(ctx)
	return agent.ClaudeLaunch{Binary: plan.Binary, Argv: plan.Argv, Env: plan.Env(os.Environ()), Set: plan.Set}, nil
}

// runValues is ctx's run snapshot with its pinned egress host, nil when ctx carries none (the live env then).
func runValues(ctx context.Context) proc.Values {
	values, ok := proc.ValuesFromContext(ctx)
	if !ok {
		return nil
	}
	if host := egress.HostContext(ctx); host != values[egress.EnvHost] {
		values = values.With(egress.EnvHost, host)
	}
	return values
}

func (claudeEngine) NewLive() agent.LiveBackend { return &claudeLive{} }

// claudeLive holds `claude --print --input-format stream-json`: one process, one user message per turn.
type claudeLive struct {
	prevCost float64
	cmd      *exec.Cmd // the held child, killed by a turn's idle watch
	row      string    // the launch profile row, for a rejected-login message
}

func (b *claudeLive) Build(ctx context.Context, req agent.RunOptions) (*exec.Cmd, *retry.TailBuffer, string, error) {
	args := claudeStreamArgs(req, true)
	cmd, tail, cl, err := claudeCmd(ctx, req, args, "")
	b.cmd, b.row = cmd, cl.Set[providers.EnvSessionProfile]
	return cmd, tail, cl.Set[providers.EnvSessionProfile], err
}

func (*claudeLive) WriteTurn(w io.Writer, prompt string) error {
	return json.NewEncoder(w).Encode(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}})
}

// Parse prices the turn alone (total_cost_usd accumulates over the process); a silent child is killed when idle.
func (b *claudeLive) Parse(dec *bufio.Reader, sink wire.Sink, lg func(kind, text string)) (wire.Result, error) {
	wd := newIdleWatch(claudeIdleLimit, func() { _ = b.cmd.Cancel() })
	wd.row = b.row
	defer wd.Stop()
	res, err := parseClaudeEvents(dec, true, wd, sink, wd.Spoke, lg)
	if serr := wd.Err(); serr != nil {
		err = wd.stalledErr(serr)
	}
	total := res.Usage.CostUSD
	res.Usage.CostUSD = max(total-b.prevCost, 0)
	b.prevCost = max(b.prevCost, total)
	return res, err
}

var _ agent.Authenticator = claudeAgent

// AuthStatus is the claude.ai login from `claude auth status`; a failed probe sets Err (unknown, not logged out).
func (claudeEngine) AuthStatus() providers.AuthState {
	ok, plan, err := claudeLoginProbe()
	if err != nil {
		return providers.AuthState{Err: err}
	}
	if !ok {
		return providers.AuthState{}
	}
	return providers.AuthState{Present: true, Source: "claude.ai login", Method: providers.AuthCLI, PlanHint: strings.Trim(plan, " ()")}
}

// Plan is the plan name ~/.claude.json shows ("Ultra"), else the rate limit ("Max 5x"), else the bare plan; offline.
func (claudeEngine) Plan(_ context.Context, st providers.AuthState) providers.Subscription {
	acct := providers.ClaudeAccount()
	raw := acct.Tier()
	if acct.PlanName != "" {
		return providers.Subscription{Raw: raw, Name: acct.PlanName}
	}
	if name := providers.MaxTierName(raw); name != "" {
		return providers.Subscription{Raw: raw, Name: name}
	}
	return providers.Subscription{Raw: raw, Name: textutil.Title(st.PlanHint)}
}

func (claudeEngine) Verify(context.Context) (providers.AuthVerdict, string) {
	ok, plan, err := claudeLoginProbe()
	switch {
	case err != nil:
		return providers.VerdictTransient, "claude auth status failed: " + err.Error()
	case ok:
		return providers.VerdictOK, strings.TrimSuffix("logged in "+strings.Trim(plan, " ()"), " ")
	}
	return providers.VerdictNeedsLogin, ""
}

// refuseCapabilities rejects a tool combination the launch cannot supply before invoking its launcher.
func refuseCapabilities(req agent.RunOptions) error {
	if req.Tools && req.NoTools {
		return retry.Fatal("claude", errors.New("--no-tools and --tools are exclusive"))
	}
	return agent.CheckCapabilities("claude", req)
}

const claudeProbeTTL = 10 * time.Second

type claudeProbe struct {
	loggedIn bool
	plan     string
}

// claudeProbeMemo is single-flight; only a logged-in answer is kept, so the recheck after a login sees it.
var claudeProbeMemo = paths.NewMemo[struct{}, claudeProbe]("claude.probe")

// claudeLoginProbe asks `claude auth status` (JSON); err != nil means unknown. Single-flight; logged-in memo 10s.
func claudeLoginProbe() (loggedIn bool, plan string, err error) {
	p, err := claudeProbeMemo.Do(struct{}{}, func(p claudeProbe, err error) time.Duration {
		if err == nil && p.loggedIn {
			return claudeProbeTTL
		}
		return -1
	}, runClaudeProbe)
	return p.loggedIn, p.plan, err
}

func runClaudeProbe() (claudeProbe, error) {
	bin, err := ClaudeBinary()
	if err != nil {
		return claudeProbe{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), agent.ProbeTimeout)
	defer cancel()
	probe := agent.ProbeContext(ctx, "claude") // claude's own login only
	out, runErr := agent.ProbeCLI(probe, bin, agent.EnvSpec{Mode: agent.EnvAgent}, false, "auth", "status")
	var st struct {
		LoggedIn         bool   `json:"loggedIn"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if jerr := json.Unmarshal(out, &st); jerr != nil { // logged-out may exit nonzero but still print JSON
		if runErr != nil {
			return claudeProbe{}, runErr
		}
		return claudeProbe{}, jerr
	}
	p := claudeProbe{loggedIn: st.LoggedIn}
	if st.SubscriptionType != "" {
		p.plan = " (" + st.SubscriptionType + ")"
	}
	return p, nil
}
