package claude

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"

	"github.com/edotau/claude-code/internal/providers"
)

// claudeEngine execs `claude -p --output-format stream-json` with the launcher's provider env and overlay.
type claudeEngine struct{ agent.BaseEngine }

// claudeAgent is the claude agent; Name and Caps come from the shared base, like every other agent.
var claudeAgent = claudeEngine{agent.NewBaseEngine("claude")}

var _ agent.Engine = claudeAgent

// Available also finds the native installer's versions dir (ClaudeBinary), which the base's AgentBin lookup does not.
func (claudeEngine) Available() error { _, err := ClaudeBinary(); return err }

func (claudeEngine) Run(ctx context.Context, req agent.RunOptions, sink wire.Sink) (wire.Result, error) {
	if req.NoTools { // the one-shot has no system-prompt or schema channel; those go through the claude process
		if req.Tools {
			return wire.Result{}, errors.New("claude: --no-tools and --tools are exclusive")
		}
		// a session must resume through the claude process: the stateless one-shot would silently drop it
		if req.SessionID == "" && req.SystemPrompt == "" && req.AppendSystemPrompt == "" && req.OutputSchema == "" && req.Persona == nil {
			if res, done, err := claudeOneShot(ctx, req, sink); done {
				return res, err
			}
		}
	}
	if err := refuseUnmappedDeny(req.Permissions); err != nil {
		return wire.Result{}, err
	}
	prompt, err := claudeAttach(req.Prompt, req.Attach, req.Tools)
	if err != nil {
		return wire.Result{}, err
	}
	return claudeExec(ctx, req, prompt, sink)
}

// claudeExec runs one stream-json claude process to completion and parses its events into sink.
func claudeExec(ctx context.Context, req agent.RunOptions, prompt string, sink wire.Sink) (wire.Result, error) {
	args := claudeArgs([]string{"--print", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}, req, anthropicLaunch(req))
	cmd, tail, cl, err := claudeCmd(ctx, req, args, prompt)
	if err != nil {
		return wire.Result{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return wire.Result{}, err
	}
	agentconfig.RecordOverlayDebug("claude", cmd.Env)
	if err := cmd.Start(); err != nil {
		return wire.Result{}, err
	}
	wd := newStallWatch(claudeIdleLimit, func() { _ = cmd.Cancel() }) // a silent child must not hold the run to --timeout
	out := &agent.EOFReader{R: stdout}
	res, perr := parseClaudeEvents(bufio.NewReader(out), false, wd, sink, wire.EventLogFor(ctx))
	wd.stop()
	werr := agent.ReapChild(cmd, out, perr)
	res.Provider = cl.Set[providers.EnvSessionProfile]
	if serr := wd.stalled(); serr != nil {
		return res, serr
	}
	if werr != nil || ctx.Err() != nil {
		return res, agent.ChildErr(ctx, "claude", errors.Join(werr, perr), tail)
	}
	return res, perr
}

// claudeArgs appends the request's option flags to base; shared by the one-shot and the live session.
func claudeArgs(args []string, req agent.RunOptions, anthropic bool) []string {
	for _, f := range []struct {
		on   bool
		flag []string
	}{
		{req.NoPersist, []string{"--no-session-persistence"}},
		{req.SafeMode, []string{"--safe-mode"}},
		{req.DebugFile != "", []string{"--debug-file", req.DebugFile}},
		{req.Effort != "", []string{"--effort", req.Effort}},
		{req.SystemPrompt != "", []string{"--system-prompt", req.SystemPrompt}},
		{req.AppendSystemPrompt != "", []string{"--append-system-prompt", req.AppendSystemPrompt}},
		{req.OutputSchema != "", []string{"--json-schema", req.OutputSchema}},
		{req.SessionID != "", []string{"--resume", req.SessionID}},
		{req.Autonomous && req.Tools && req.Permissions.IsZero(), []string{"--permission-mode", "bypassPermissions"}},
	} {
		if f.on {
			args = append(args, f.flag...)
		}
	}
	if req.Persona != nil {
		args = append(args, agent.PersonaArgs(req.Persona)...)
	}
	args = append(args, permissionArgs(req, anthropic)...)
	if !req.Tools {

		args = append(args, "--tools", "", "--setting-sources", "")
	} else {
		args = append(args, "--exclude-dynamic-system-prompt-sections") // per-session sections defeat the prompt cache
	}
	return args
}

// planDeny is plan mode's write boundary: deny beats the user's allow rules and defaultMode, which a bare plan mode does not.
var planDeny = []agent.Rule{{Tool: "Edit"}, {Tool: "Write"}, {Tool: "NotebookEdit"}, {Tool: "Bash"}}

// gatewayAllow is a rule-less --tools leaf's allow list off Anthropic, where dontAsk denies whatever no rule matches.
var gatewayAllow = func() []agent.Rule {
	rules := []agent.Rule{{Tool: "Edit"}, {Tool: "Write"}, {Tool: "Read"}, {Tool: "Grep"}, {Tool: "Glob"}}
	for _, c := range []string{"go build *", "go vet *", "go test *", "gofmt *", "go run *", "git diff *", "git status *",
		"git log *", "git show *", "jq *", "ls *", "cat *", "rg *", "grep *"} {
		rules = append(rules, agent.Rule{Tool: "Bash", Arg: c})
	}
	return rules
}()

// permissionArgs renders the request's Permissions as claude flags; empty for none.
// Mode plan adds planDeny and sets no --permission-mode: --permission-prompts none denies what no rule matched.
func permissionArgs(req agent.RunOptions, anthropic bool) []string {
	p := req.Permissions
	switch {
	case !p.IsZero():
	case req.Tools && !req.Autonomous && !anthropic: // settings.json's defaultMode auto would call the unserved judge
		p.Allow = gatewayAllow
	default:
		return nil
	}
	if p.Mode == "plan" && req.Tools {
		p.Deny = append(slices.Clone(p.Deny), planDeny...)
	}
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
	switch {
	case p.Mode == "plan":
	case anthropic: // D3 auto: Ask and unmatched calls go to claude's judge; deny stays the hard boundary
		args = append(args, "--permission-mode", "auto")
	default: // auto's judge calls a fixed Anthropic model other providers don't serve, so unmatched calls are denied
		args = append(args, "--permission-mode", "dontAsk")
	}
	return append(args, "--permission-prompts", "none")
}

// refuseUnmappedDeny fails closed: claude takes any "Tool(arg)" rule natively, so only a nameless Deny rule cannot be enforced.
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

// anthropicLaunch is true when req launches on an Anthropic first-party profile, the only one serving auto mode's judge.
func anthropicLaunch(req agent.RunOptions) bool {
	reg, err := providers.Load()
	if err != nil {
		return false
	}
	p, err := reg.Get(launchProvider(req))
	return err == nil && p.Provider == "anthropic"
}

// claudeCmd resolves the launch plan for args and returns the unstarted child with its env layered (--env last).
func claudeCmd(ctx context.Context, req agent.RunOptions, args []string, prompt string) (*exec.Cmd, *retry.TailBuffer, agent.ClaudeLaunch, error) {
	if req.Launcher == nil {
		return nil, nil, agent.ClaudeLaunch{}, errors.New("claude: no launcher (caller must set RunOptions.Launcher)")
	}
	o := agent.ClaudeOptions{Provider: launchProvider(req), Model: req.Model, KeepEnv: req.KeepEnv}
	cl, err := req.Launcher.Claude(ctx, o, args)
	if err != nil {
		return nil, nil, cl, err
	}
	if err := agent.CheckRouteNames(launcherSet(cl.Set), req.OverlaySet); err != nil {
		return nil, nil, cl, err
	}
	cmd, tail := agent.ChildCmd(ctx, cl.Binary, cl.Argv, nil, req.WorkDir, prompt, req.KeepEnv, false)
	keep := append(slices.Clone(req.KeepEnv), slices.Collect(maps.Keys(cl.Set))...)
	cmd.Env = agent.EnvSpec{Mode: agent.EnvPrescrubbed, Base: cl.Env, Keep: keep, Set: launcherSet(cl.Set), Extra: req.Env}.Environ(ctx) // --env last, so it wins
	return cmd, tail, cl, nil
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

// topMessage decodes a top-level assistant or user message, the only ones the stream state and stall watch read.
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

// parseClaudeStream emits top-level assistant text and tool calls to sink and returns the result event's envelope.
func parseClaudeStream(br *bufio.Reader, sink wire.Sink, logs ...wire.StreamLog) (res wire.Result, _ error) {
	return parseClaudeEvents(br, false, nil, sink, logs...)
}

// parseClaudeEvents stops at the first result when untilResult is set, preserving buffered lines for the next turn.
func parseClaudeEvents(br *bufio.Reader, untilResult bool, wd *stallWatch, sink wire.Sink, logs ...wire.StreamLog) (res wire.Result, _ error) {
	lg := wire.PickLog(logs)
	sawResult := false
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
	case "error":
		if t := ev.text(); t != "" {
			_ = wire.EmitError(s.log, s.sink, t)
			return false, fmt.Errorf("claude: %s", t)
		}
	}
	return false, nil
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
		res.Model = string(msg.Model)
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

func claudeAttach(prompt string, paths []string, tools bool) (string, error) {
	images, rest := agent.SplitImages(paths)
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

// launcherSet is the launch's own route values as KEY=VALUE, re-added after Unset so an overlay never drops them.
func launcherSet(set map[string]string) []string {
	out := make([]string, 0, len(set))
	for _, k := range slices.Sorted(maps.Keys(set)) {
		out = append(out, k+"="+set[k])
	}
	return out
}
