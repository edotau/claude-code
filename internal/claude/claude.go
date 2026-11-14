package claude

import (
	"bufio"
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

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

// claudeEngine execs `claude -p --output-format stream-json` with the launcher's provider env and overlay.
type claudeEngine struct{ agent.BaseEngine }

// claudeAgent is the claude agent; Name and Caps come from the shared base, like every other agent.
var claudeAgent = claudeEngine{agent.NewBaseEngine("claude")}

var _ agent.Engine = claudeAgent

// Available also finds the native installer's versions dir (ClaudeBinary), which the base's AgentBin lookup does not.
func (claudeEngine) Available() error { _, err := ClaudeBinary(); return err }

func (claudeEngine) Run(ctx context.Context, req agent.RunOptions, sink wire.Sink) (wire.Result, error) {
	if err := refusePermissionCapabilities(req); err != nil {
		return wire.Result{}, err
	}
	if req.NoTools {
		if res, done, err := runNoTools(ctx, req, sink); done {
			return res, err
		}
	}
	if err := refuseUnmappedDeny(req.Permissions); err != nil {
		return wire.Result{}, err
	}
	if req.ReadOnly && req.SafeMode { // --safe-mode loads no hooks, so nothing would limit Bash to the read-only list
		return wire.Result{}, retry.Fatal("claude", errors.New("--read-only cannot run under --safe-mode: the shell gate is a hook"))
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
	if req.SessionID != "" || req.SystemPrompt != "" || req.AppendSystemPrompt != "" || req.OutputSchema != "" || req.Persona != nil {
		return wire.Result{}, false, nil
	}
	return claudeOneShot(ctx, req, sink)
}

// claudeExec runs one stream-json claude process to completion and parses its events into sink.
func claudeExec(ctx context.Context, req agent.RunOptions, prompt string, sink wire.Sink) (wire.Result, error) {
	ctx, req = gateOffAnthropic(ctx, req)
	args := claudeArgs([]string{"--print", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}, req)
	cmd, tail, cl, err := claudeCmd(ctx, req, args, prompt)
	if err != nil {
		return wire.Result{}, err
	}
	wd := newIdleWatch(claudeIdleLimit, nil) // RunEventStream binds the kill once the child starts
	parse := func(br *bufio.Reader, sink wire.Sink, spoke func(), logs ...wire.StreamLog) (wire.Result, error) {
		return parseClaudeEvents(br, false, wd, sink, spoke, logs...)
	}
	res, err := agent.RunEventStream(ctx, "claude", cmd, tail, sink, parse, agent.StreamOpts{Idle: wd.IdleTimeout})
	res.Provider = cl.Set[providers.EnvSessionProfile]
	return res, err
}

// claudeArgs appends the request's option flags to base; effort and permissions are RenderSettings', tools toolArgs'.
func claudeArgs(args []string, req agent.RunOptions) []string {
	for _, f := range []struct {
		on   bool
		flag []string
	}{
		{req.NoPersist, []string{"--no-session-persistence"}},
		{req.SafeMode, []string{"--safe-mode"}},
		{req.DebugFile != "", []string{"--debug-file", req.DebugFile}},
		{req.SystemPrompt != "", []string{"--system-prompt", req.SystemPrompt}},
		{req.AppendSystemPrompt != "", []string{"--append-system-prompt", req.AppendSystemPrompt}},
		{req.OutputSchema != "", []string{"--json-schema", req.OutputSchema}},
		{req.SessionID != "", []string{"--resume", req.SessionID}},
		{req.Tools && (req.Gate || req.Autonomous && req.Permissions.IsZero()), []string{"--permission-mode", "bypassPermissions"}},
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

// planDeny is plan mode's write boundary: deny beats the user's allow rules and defaultMode, unlike bare plan mode.
var planDeny = []agent.Rule{{Tool: "Edit"}, {Tool: "Write"}, {Tool: "NotebookEdit"}, {Tool: "Bash"}}

// RenderSettings adds --effort and, headless, the permission flags after the base launch env/args.
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
	if !s.Headless { // an interactive launch keeps the user's own permission settings
		return env, argv, emulated, cleanup, nil
	}
	if s.Gate { // bypassPermissions plus the gate hook (gateArgs) replace claude's own rules
		return env, argv, emulated, cleanup, nil
	}
	if err := refuseUnmappedDeny(s.Permissions); err != nil {
		return env, argv, emulated, cleanup, err
	}
	return env, append(argv, permissionArgs(s.Permissions, s.Tools, s.Autonomous, onAnthropic(s.Target))...), emulated, cleanup, nil
}

// claudeEffort spells e for claude's CLI and Messages API; "" stays "" and an unknown effort is refused.
func claudeEffort(e string) (string, error) {
	if err := agentid.ValidEffort(agentid.Claude, e); err != nil {
		return "", err
	}
	native, _ := agentid.NativeEffort(agentid.Claude, agentid.Effort(e))
	return native, nil
}

// permissionArgs renders the request's Permissions as claude flags; empty for none.
// Mode plan adds planDeny and sets no --permission-mode: --permission-prompts none denies what no rule matched.
func permissionArgs(p agent.Permissions, tools, autonomous, anthropic bool) []string {
	switch {
	case tools && !anthropic && p.Mode != "plan" && (!autonomous || !p.IsZero()): // dontAsk: the request's rules only add
		p.Allow = slices.Concat(gatewayAllow(), p.Allow)
	case p.IsZero():
		return nil
	}
	if p.Mode == "plan" && tools {
		p.Deny = append(slices.Clone(p.Deny), planDeny...)
	}
	return append(append(ruleArgs(p), permissionModeArgs(p.Mode, anthropic)...), "--permission-prompts", "none")
}

// ruleArgs is --allowedTools and --disallowedTools with their rules, each omitted when empty.
func ruleArgs(p agent.Permissions) []string {
	var args []string
	for _, l := range []struct {
		flag  string
		rules []agent.Rule
	}{{"--allowedTools", p.Allow}, {"--disallowedTools", p.Deny}} {
		if len(l.rules) > 0 {
			args = append(args, l.flag)
			for _, r := range l.rules {
				args = append(args, r.String())
			}
		}
	}
	return args
}

// permissionModeArgs is the --permission-mode pair; plan sets none.
func permissionModeArgs(mode string, anthropic bool) []string {
	switch {
	case mode == "plan":
		return nil
	case anthropic: // D3 auto: Ask and unmatched calls go to claude's judge; deny stays the hard boundary
		return []string{"--permission-mode", "auto"}
	default: // auto's judge calls a fixed Anthropic model other providers don't serve, so unmatched calls are denied
		return []string{"--permission-mode", "dontAsk"}
	}
}

// refuseUnmappedDeny fails closed: claude takes any "Tool(arg)" rule, so only a nameless Deny rule cannot be enforced.
func refuseUnmappedDeny(p agent.Permissions) error {
	for _, r := range p.Deny {
		if r.Tool == "" {
			return retry.Fatal("claude", fmt.Errorf("cannot enforce deny rule %q: no tool name", r.String()))
		}
	}
	return nil
}

// launchProvider is the profile name claudeCmd launches req on.
func launchProvider(req agent.RunOptions) string {
	return cmp.Or(req.Provider, agent.SessionProvider("claude"))
}

// launchTarget is req's launch profile row; Provider nil when it does not resolve (never Anthropic, so no auto judge).
func launchTarget(req agent.RunOptions) providers.Target {
	t := providers.Target{Model: req.Model}
	if reg, err := providers.Load(); err == nil {
		if p, err := reg.Get(launchProvider(req)); err == nil {
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
	st, err := agent.ResolveSettings(agentid.Claude, req, launchTarget(req), true)
	if err != nil {
		return nil, nil, agent.ClaudeLaunch{}, retry.Fatal("claude", err)
	}
	launchEnv, args, _, _, err := claudeAgent.RenderSettings(ctx, st, nil, args) // the cleanup is the base's no-op
	if err != nil {
		return nil, nil, agent.ClaudeLaunch{}, retry.Fatal("claude", err)
	}
	args = append(args, toolArgs(req)...)
	if req.Gate {
		args = append(args, gateArgs(req)...)
	}
	if err := refuseUngatedBypass(args, req); err != nil {
		return nil, nil, agent.ClaudeLaunch{}, err
	}
	o := agent.ClaudeOptions{Provider: launchProvider(req), Model: req.Model, KeepEnv: req.KeepEnv}
	cl, err := req.Launcher.Claude(ctx, o, args)
	if err != nil {
		return nil, nil, cl, err
	}
	if req.Gate {
		if cl.Argv, err = injectGate(cl.Argv, req); err != nil {
			return nil, nil, cl, err
		}
	}
	if err := agent.CheckRouteNames(proc.Values(cl.Set).Env(), req.OverlaySet, req.OverlayUnset); err != nil {
		return nil, nil, cl, err
	}
	cmd, tail, err := claudeChild(ctx, req, cl, launchEnv, prompt)
	return cmd, tail, cl, err
}

// refuseClaudeCmd is the refusal claudeCmd makes before it resolves anything: a permission it cannot enforce, no launcher.
func refuseClaudeCmd(req agent.RunOptions) error {
	if err := refusePermissionCapabilities(req); err != nil {
		return err
	}
	if req.Launcher == nil {
		return errors.New("claude: no launcher (caller must set RunOptions.Launcher)")
	}
	return nil
}

// claudeChild is the unstarted child for the resolved launch, its --env applied last, refused if the gate env leaked.
func claudeChild(ctx context.Context, req agent.RunOptions, cl agent.ClaudeLaunch, launchEnv []string, prompt string) (*exec.Cmd, *retry.TailBuffer, error) {
	cmd, tail := agent.ChildCmd(ctx, cl.Binary, cl.Argv, nil, req.WorkDir, prompt, req.KeepEnv, false)
	keep := append(slices.Clone(req.KeepEnv), slices.Collect(maps.Keys(cl.Set))...)
	set := agent.RouteEnvLast(dropLaunchOwned(launchEnv), proc.Values(cl.Set).Env())
	cmd.Env = agent.EnvSpec{Mode: agent.EnvPrescrubbed, Base: cl.Env, Keep: keep, Set: set, Extra: req.Env}.Environ(ctx) // --env last, so it wins
	if err := refuseGateEnv(req, cmd.Env); err != nil {
		return nil, nil, err
	}
	return cmd, tail, nil
}

// gateExe is the binary the injected gate runs; a var so tests pin it.
var gateExe = os.Executable

// gateTools is the only built-in tool set a gated child gets: the permission-gate judges exactly these.
const gateTools = "Read,Glob,Grep,Edit,Write,MultiEdit,Bash"

// readOnlyGateTools is the read-only gated child's tool set: reads plus Bash, which the gate limits to the shared list.
const readOnlyGateTools = "Read,Glob,Grep,Bash"

// gateArgs are the launch flags of a gated child: no MCP and only the tools the gate judges.
func gateArgs(req agent.RunOptions) []string {
	if req.ReadOnly {
		return []string{"--strict-mcp-config", "--tools", readOnlyGateTools}
	}
	return []string{"--strict-mcp-config", "--tools", gateTools}
}

// injectGate merges the permission-gate hook into the launch's one --settings (a second flag would replace it).
func injectGate(argv []string, req agent.RunOptions) ([]string, error) {
	// the hook exits 2 on any failure, so a gate that cannot run blocks the call; no policy or binary refuses the launch
	refuse := func(err error) ([]string, error) {
		return nil, retry.Fatal("claude", fmt.Errorf("gated launch refused: %w", err))
	}
	if err := refuseGate(req); err != nil {
		return refuse(err)
	}
	exe, err := gateExe()
	if err == nil {
		_, err = os.Stat(exe)
	}
	if err != nil {
		return refuse(fmt.Errorf("permission-gate binary: %w", err))
	}
	doc := map[string]any{}
	at := slices.Index(argv, "--settings")
	if at >= 0 && at+1 < len(argv) {
		raw := []byte(argv[at+1])
		if !strings.HasPrefix(strings.TrimSpace(argv[at+1]), "{") {
			if raw, err = os.ReadFile(argv[at+1]); err != nil {
				return refuse(fmt.Errorf("launch settings: %w", err))
			}
		}
		if err = json.Unmarshal(raw, &doc); err != nil {
			return refuse(fmt.Errorf("launch settings: %w", err))
		}
	}
	doc["hooks"] = map[string]any{"PreToolUse": []any{map[string]any{"hooks": []any{map[string]any{
		"type": "command", "timeout": 5, "command": textutil.ShellQuote(exe) + " hook PreToolUse --permission-gate || exit 2"}}}}}
	merged, err := json.Marshal(doc)
	if err != nil {
		return refuse(fmt.Errorf("gate settings: %w", err))
	}
	if at < 0 {
		return append(slices.Clone(argv), "--settings", string(merged)), nil
	}
	out := append(slices.Clone(argv[:at+1]), string(merged)) // a bare trailing --settings gets the document as its value
	return append(out, argv[min(at+2, len(argv)):]...), nil
}

// claudeEvent is the union of stream-json fields the agent reads.
// Message is raw: most event types carry it as an object, but error/system/result-type
// events sometimes carry it as a plain string (e.g. "system","subtype":"permission_denied") —
// typing it strictly broke decode of the whole stream on one odd event.
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
	TotalCostUSD    float64         `json:"total_cost_usd"`
	Structured      json.RawMessage `json:"structured_output"`
	Usage           struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// decodeMessage decodes a raw message as an anthropic.Message (string or absent = zero); a var so tests count calls.
var decodeMessage = func(raw json.RawMessage) anthropic.Message {
	var m anthropic.Message
	if len(raw) == 0 || raw[0] != '{' {
		return m
	}
	_ = json.Unmarshal(raw, &m) // best-effort: an odd shape must never fail the stream
	return m
}

// topMessage decodes a top-level assistant or user message, the only ones the stream state and idle watch read.
func (ev claudeEvent) topMessage() anthropic.Message {
	if ev.ParentToolUseID != "" || (ev.Type != "assistant" && ev.Type != "user") {
		return anthropic.Message{}
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
	seen := wire.Answer{Join: "\n\n"} // top-level assistant text, the answer when no result event delivers one
	partial := func(err error) (wire.Result, error) {
		if res.Answer == "" {
			res.Answer = seen.String()
		}
		return res, err
	}
	tr, err := wire.DecodeEvents(br, lg, "decode stream-json", false, nil, func(ev claudeEvent, ts *wire.TransportTracker) (bool, error) {
		msg := ev.topMessage()
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
		state := claudeStreamState{res: &res, transport: ts, log: lg, sink: sink, seen: &seen}
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
}

func (s claudeStreamState) apply(ev claudeEvent, msg anthropic.Message, untilResult bool) (bool, error) {
	switch ev.Type {
	case "stream_event":
		s.transport.Event(wire.Clock())
	case "system":
		return false, applyClaudeSystem(ev, s.res, s.transport, s.log, s.sink)
	case "assistant":
		return false, applyClaudeAssistantEvent(ev, msg, s.res, s.log, s.sink, s.seen)
	case "result":
		if err := applyClaudeResult(s.res, ev); err != nil {
			_ = wire.EmitError(s.log, s.sink, err.Error())
			return false, err
		}
		s.log.Emit("result", ev.Result)
		return untilResult, nil
	case "user":
		s.res.GateBlocks += ev.gateBlocks()
	case "error":
		if t := ev.text(); t != "" {
			_ = wire.EmitError(s.log, s.sink, t)
			return false, fmt.Errorf("claude: %s", t)
		}
	}
	return false, nil
}

// gateBlockPrefix starts every Reason the permission-gate hook blocks with (internal/hooks/safety/permission.go).
const gateBlockPrefix = "permission-gate: BLOCKED"

// gateBlocks counts a top-level user event's is_error tool_results whose text (string or text parts) is a gate block.
func (ev claudeEvent) gateBlocks() (n int) {
	if ev.ParentToolUseID != "" {
		return 0
	}
	var m struct {
		Content []struct {
			Type    string          `json:"type"`
			IsError bool            `json:"is_error"`
			Content json.RawMessage `json:"content"`
		} `json:"content"`
	}
	if json.Unmarshal(ev.Message, &m) != nil { // a string or odd message carries no blocks
		return 0
	}
	for _, b := range m.Content {
		if b.Type == "tool_result" && b.IsError && strings.Contains(toolResultText(b.Content), gateBlockPrefix) {
			n++
		}
	}
	return n
}

// toolResultText flattens a tool_result's content, a string or an array of text parts.
func toolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts) // best-effort: an odd shape counts as no text
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p.Text)
	}
	return sb.String()
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
func applyClaudeAssistantEvent(ev claudeEvent, msg anthropic.Message, res *wire.Result, lg wire.StreamLog, sink wire.Sink, seen *wire.Answer) error {
	if ev.ParentToolUseID != "" {
		return nil
	}
	if msg.Model != "" {
		res.Model = msg.Model
	}
	logClaudeAssistant(lg, msg)
	return emitClaudeAssistant(sink, msg, seen)
}

func logClaudeAssistant(lg wire.StreamLog, msg anthropic.Message) {
	for _, b := range msg.Content {
		switch b := b.AsAny().(type) {
		case anthropic.TextBlock:
			lg.Emit("assistant", b.Text)
		case anthropic.ToolUseBlock:
			lg.Emit("tool", b.Name)
		}
	}
}

// emitClaudeAssistant emits each text block through seen and each tool call to sink.
func emitClaudeAssistant(sink wire.Sink, msg anthropic.Message, seen *wire.Answer) error {
	for _, b := range msg.Content {
		var err error
		switch b := b.AsAny().(type) {
		case anthropic.ToolUseBlock:
			err = sink.Emit(wire.ToolEvent(b.Name, b.ID))
		case anthropic.TextBlock:
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
	if ev.Result != "" && !ev.IsError { // an error result's text rides in the error, not over the partial answer
		res.Answer = ev.Result // a resumed session's closing empty result must not erase the last real one
	}
	u := ev.Usage
	res.Usage = wire.Usage{InputTokens: u.InputTokens, CacheCreationTokens: u.CacheCreationInputTokens,
		CacheReadTokens: u.CacheReadInputTokens, OutputTokens: u.OutputTokens, CostUSD: ev.TotalCostUSD}
	if len(ev.Structured) > 0 && string(ev.Structured) != "null" {
		res.Structured = ev.Structured
	}
	if ev.IsError {
		return wire.NewResultError(fmt.Errorf("claude %s: %s", ev.Subtype, ev.Result))
	}
	return nil
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
		sb.WriteString("\n\nAttached image (open it with the Read tool): ")
		sb.WriteString(p)
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
	Caps: agent.Caps{Model: agent.Native, Provider: agent.Native, Effort: agent.Native, WorkDir: agent.Native, SessionID: agent.Native,
		Tools: agent.Native, NoTools: agent.Native, Autonomous: agent.Native, NoPersist: agent.Native, SafeMode: agent.Native, DebugFile: agent.Native,
		SystemPrompt: agent.Native, AppendSystemPrompt: agent.Native, Env: agent.Native, OutputSchema: agent.Native, Persona: agent.Native,
		Permissions: agent.Native, ReadOnly: agent.Emulated},
	Events: []string{wire.EventStart, wire.EventText, wire.EventToolUse, wire.EventRetry, wire.EventResult, wire.EventError},
	Cost:   true, ConfigureHint: "run `claude-code configure agents`", LoginArgv: []string{"claude", "auth", "login"},
}}

// claudeLauncher implements Launcher over Claude: the plan's overlay is written, its env layered on the process env.
type claudeLauncher struct{}

func (claudeLauncher) Claude(ctx context.Context, o agent.ClaudeOptions, args []string) (agent.ClaudeLaunch, error) {
	plan, err := claudePlan(ctx, agent.LaunchOptions{Provider: o.Provider, Model: o.Model, KeepEnv: o.KeepEnv}, args)
	if err != nil {
		return agent.ClaudeLaunch{}, err
	}
	if err := plan.WriteFiles(); err != nil {
		return agent.ClaudeLaunch{}, err
	}
	return agent.ClaudeLaunch{Binary: plan.Binary, Argv: plan.Argv, Env: plan.Env(os.Environ()), Set: plan.Set}, nil
}

func (claudeEngine) NewLive() agent.LiveBackend { return &claudeLive{} }

// claudeLive holds `claude --print --input-format stream-json`: one process, one user message per turn.
type claudeLive struct {
	prevCost float64
	cmd      *exec.Cmd // the held child, killed by a turn's idle watch
}

func (b *claudeLive) Build(ctx context.Context, req agent.RunOptions) (*exec.Cmd, *retry.TailBuffer, string, error) {
	ctx, req = gateOffAnthropic(ctx, req)
	args := claudeArgs([]string{"--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages"}, req)
	cmd, tail, cl, err := claudeCmd(ctx, req, args, "")
	b.cmd = cmd
	return cmd, tail, cl.Set[providers.EnvSessionProfile], err
}

func (*claudeLive) WriteTurn(w io.Writer, prompt string) error {
	return agent.WriteJSONLine(w, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}})
}

// Parse prices the turn alone (total_cost_usd accumulates over the process); a silent child is killed when idle.
func (b *claudeLive) Parse(dec *bufio.Reader, sink wire.Sink, lg func(kind, text string)) (wire.Result, error) {
	wd := newIdleWatch(claudeIdleLimit, func() { _ = b.cmd.Cancel() })
	defer wd.Stop()
	res, err := parseClaudeEvents(dec, true, wd, sink, wd.Spoke, lg)
	if serr := wd.Err(); serr != nil {
		err = serr
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

// claudeProbeTTL is how long a logged-in `claude auth status` result is reused.
const claudeProbeTTL = 10 * time.Second

// claudeProbe is one `claude auth status` answer.
type claudeProbe struct {
	loggedIn bool
	plan     string
}

// claudeProbeMemo is single-flight; only a logged-in answer is kept, so the recheck after a login sees it.
var claudeProbeMemo paths.Memo[struct{}, claudeProbe]

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

// runClaudeProbe spawns `claude auth status` (≤8s) and parses its JSON.
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
