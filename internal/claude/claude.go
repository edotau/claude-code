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
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent"

	"github.com/edotau/claude-code/internal/providers"
)

// claudeEngine execs `claude -p --output-format stream-json` with the launcher's provider env and overlay.
type claudeEngine struct{ agent.BaseEngine }

// claudeAgent is the claude agent; Name and Caps come from the shared base, like every other agent.
var claudeAgent = claudeEngine{agent.NewBaseEngine("claude")}

var _ agent.Engine = claudeAgent

// Available also finds the native installer's versions dir (ClaudeBinary), which the base's AgentBin lookup does not.
func (claudeEngine) Available() error { _, err := ClaudeBinary(); return err }

func (claudeEngine) Run(ctx context.Context, req agent.RunOptions, sink agent.Sink) (agent.Result, error) {
	if req.NoTools { // the one-shot has no system-prompt or schema channel; those go through the claude process
		if req.Tools {
			return agent.Result{}, errors.New("claude: --no-tools and --tools are exclusive")
		}
		if req.SystemPrompt == "" && req.AppendSystemPrompt == "" && req.OutputSchema == "" && req.Persona == nil {
			if res, done, err := claudeOneShot(ctx, req, sink); done {
				return res, err
			}
		}
	}
	prompt, err := claudeAttach(req.Prompt, req.Attach, req.Tools)
	if err != nil {
		return agent.Result{}, err
	}
	return claudeExec(ctx, req, prompt, sink)
}

// claudeExec runs one stream-json claude process to completion and parses its events into sink.
func claudeExec(ctx context.Context, req agent.RunOptions, prompt string, sink agent.Sink) (agent.Result, error) {
	args := claudeArgs([]string{"--print", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}, req)
	cmd, tail, cl, err := claudeCmd(ctx, req, args, prompt)
	if err != nil {
		return agent.Result{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agent.Result{}, err
	}
	agent.RecordOverlayDebug("claude", cmd.Env)
	if err := cmd.Start(); err != nil {
		return agent.Result{}, err
	}
	res, perr := parseClaudeStream(bufio.NewReader(stdout), sink, agent.EventLogFor(ctx))
	if perr != nil {
		_ = cmd.Cancel() // a dead consumer or bad stream must not leave claude running to --timeout
	} else {
		_, _ = io.Copy(io.Discard, stdout) // no drain after a kill: a grandchild holding stdout would block it; Wait closes it
	}
	werr := cmd.Wait()
	res.Provider = cl.Set[providers.EnvSessionProfile]
	if werr != nil || ctx.Err() != nil {
		return res, agent.ChildErr(ctx, "claude", errors.Join(werr, perr), tail)
	}
	return res, perr
}

// claudeArgs appends the request's option flags to base; shared by the one-shot and the live session.
func claudeArgs(args []string, req agent.RunOptions) []string {
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
		{req.Autonomous && req.Tools, []string{"--permission-mode", "bypassPermissions"}},
	} {
		if f.on {
			args = append(args, f.flag...)
		}
	}
	if req.Persona != nil {
		args = append(args, agent.PersonaArgs(req.Persona)...)
	}
	if !req.Tools {

		args = append(args, "--tools", "", "--setting-sources", "")
	} else {
		args = append(args, "--exclude-dynamic-system-prompt-sections") // per-session sections defeat the prompt cache
	}
	return args
}

// claudeCmd resolves the launch plan for args and returns the unstarted child with its env layered (--env last).
func claudeCmd(ctx context.Context, req agent.RunOptions, args []string, prompt string) (*exec.Cmd, *agent.TailBuffer, agent.ClaudeLaunch, error) {
	if req.Launcher == nil {
		return nil, nil, agent.ClaudeLaunch{}, errors.New("claude: no launcher (caller must set RunOptions.Launcher)")
	}
	o := agent.ClaudeOptions{Provider: cmp.Or(req.Provider, agent.SessionProvider("claude")), Model: req.Model, KeepEnv: req.KeepEnv}
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

// claudeMessage is the object shape of claudeEvent.Message, for assistant/user events only.
type claudeMessage struct {
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Name string `json:"name"`
		ID   string `json:"id"`
	} `json:"content"`
}

// object decodes ev.Message as a claudeMessage; a string or absent message yields the zero value.
func (ev claudeEvent) object() claudeMessage {
	var m claudeMessage
	if len(ev.Message) == 0 || ev.Message[0] != '{' {
		return m
	}
	_ = json.Unmarshal(ev.Message, &m) // best-effort: an odd shape must never fail the stream
	return m
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
func parseClaudeStream(br *bufio.Reader, sink agent.Sink, logs ...agent.StreamLog) (res agent.Result, _ error) {
	return parseClaudeEvents(br, false, sink, logs...)
}

// parseClaudeEvents stops at the first result when untilResult is set, preserving buffered lines for the next turn.
func parseClaudeEvents(br *bufio.Reader, untilResult bool, sink agent.Sink, logs ...agent.StreamLog) (res agent.Result, _ error) {
	lg := agent.PickLog(logs)
	sawResult, wrote := false, false
	ts := agent.TransportTracker{Start: agent.Clock()}
	defer func() { res.Transport = ts.Result() }()
	err := agent.DecodeLines(br, lg, func(raw []byte) (bool, error) {
		var ev claudeEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return false, fmt.Errorf("decode stream-json: %w", err)
		}
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		switch ev.Type {
		case "stream_event":
			ts.Event(agent.Clock())
		case "system":
			return false, applyClaudeSystem(ev, &res, &ts, lg, sink)
		case "assistant":
			return false, applyClaudeAssistantEvent(ev, &res, lg, sink, &wrote)
		case "result":
			sawResult = true
			if err := applyClaudeResult(&res, ev); err != nil {
				_ = agent.EmitError(lg, sink, err.Error())
				return false, err
			}
			lg.Emit("result", ev.Result)
			if untilResult {
				return true, nil
			}
		case "error":
			if t := ev.text(); t != "" {
				_ = agent.EmitError(lg, sink, t)
				return false, fmt.Errorf("claude: %s", t)
			}
		}
		return false, nil
	})
	if err != nil {
		return res, err
	}
	if !sawResult {
		return res, errNoResult
	}
	return res, nil
}

var errNoResult = errors.New("claude: stream ended without a result event")

// applyClaudeSystem records an api_retry and the session model from a system event.
func applyClaudeSystem(ev claudeEvent, res *agent.Result, ts *agent.TransportTracker, lg agent.StreamLog, sink agent.Sink) error {
	if ev.Subtype == "api_retry" {
		ts.Retry(ev.ErrorStatus)
		lg.Emit("retry", fmt.Sprintf("attempt %d/%d status=%d retry_in=%dms",
			ev.Attempt, ev.MaxRetries, ts.LastStatus(), ev.RetryDelayMs))
		if err := sink.Emit(agent.Event{Type: agent.EventRetry, Status: strconv.Itoa(ts.LastStatus()),
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
func applyClaudeAssistantEvent(ev claudeEvent, res *agent.Result, lg agent.StreamLog, sink agent.Sink, wrote *bool) error {
	if ev.ParentToolUseID != "" {
		return nil
	}
	msg := ev.object()
	if msg.Model != "" {
		res.Model = msg.Model
	}
	logClaudeAssistant(lg, msg)
	return emitClaudeAssistant(sink, msg, wrote)
}

func logClaudeAssistant(lg agent.StreamLog, msg claudeMessage) {
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			lg.Emit("assistant", b.Text)
		case "tool_use":
			lg.Emit("tool", b.Name)
		}
	}
}

func emitClaudeAssistant(sink agent.Sink, msg claudeMessage, wrote *bool) error {
	for _, b := range msg.Content {
		var ev agent.Event
		switch {
		case b.Type == "tool_use":
			ev = agent.ToolEvent(b.Name, b.ID)
		case b.Type == "text" && b.Text != "":
			ev = agent.Event{Type: agent.EventText, Text: b.Text, Sep: *wrote}
			*wrote = true
		default:
			continue
		}
		if err := sink.Emit(ev); err != nil {
			return err
		}
	}
	return nil
}

func applyClaudeResult(res *agent.Result, ev claudeEvent) error {
	if ev.Result != "" {
		res.Answer = ev.Result // a resumed session's closing empty result must not erase the last real one
	}
	u := ev.Usage
	res.Usage = agent.Usage{InputTokens: u.InputTokens, CacheCreationTokens: u.CacheCreationInputTokens,
		CacheReadTokens: u.CacheReadInputTokens, OutputTokens: u.OutputTokens, CostUSD: ev.TotalCostUSD}
	if len(ev.Structured) > 0 && string(ev.Structured) != "null" {
		res.Structured = ev.Structured
	}
	if ev.IsError {
		return agent.NewResultError(fmt.Errorf("claude %s: %s", ev.Subtype, ev.Result))
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
