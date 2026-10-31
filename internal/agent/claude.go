package agent

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

	"github.com/edotau/claude-code/internal/providers"
)

// claudeRunner execs `claude -p --output-format stream-json` with the launcher's provider env and overlay.
type claudeRunner struct{ baseEngine }

// claudeAgent is the claude runner; Name and Caps come from the shared base, like every other agent.
var claudeAgent = claudeRunner{baseEngine{"claude"}}

var _ Runner = claudeAgent

// Available also finds the native installer's versions dir (ClaudeBinary), which the base's VendorBin lookup does not.
func (claudeRunner) Available() error { _, err := ClaudeBinary(); return err }

func (claudeRunner) Run(ctx context.Context, req RunOptions, sink Sink) (Result, error) {
	if req.NoTools { // the one-shot has no system-prompt or schema channel; those go through the claude process
		if req.Tools {
			return Result{}, errors.New("claude: --no-tools and --tools are exclusive")
		}
		if req.SystemPrompt == "" && req.AppendSystemPrompt == "" && req.OutputSchema == "" && req.Persona == nil {
			if res, done, err := claudeOneShot(ctx, req, sink); done {
				return res, err
			}
		}
	}
	prompt, err := claudeAttach(req.Prompt, req.Attach, req.Tools)
	if err != nil {
		return Result{}, err
	}
	return claudeExec(ctx, req, prompt, sink)
}

// claudeExec runs one stream-json claude process to completion and parses its events into sink.
func claudeExec(ctx context.Context, req RunOptions, prompt string, sink Sink) (Result, error) {
	args := claudeArgs([]string{"--print", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}, req)
	cmd, tail, cl, err := claudeCmd(ctx, req, args, prompt)
	if err != nil {
		return Result{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	res, perr := parseClaudeStream(bufio.NewReader(stdout), sink, eventLogFor(ctx))
	if perr != nil {
		_ = cmd.Cancel() // a dead consumer or bad stream must not leave claude running to --timeout
	} else {
		_, _ = io.Copy(io.Discard, stdout) // no drain after a kill: a grandchild holding stdout would block it; Wait closes it
	}
	werr := cmd.Wait()
	res.Provider = cl.Set[providers.EnvSessionProfile]
	if werr != nil || ctx.Err() != nil {
		return res, childErr(ctx, "claude", errors.Join(werr, perr), tail)
	}
	return res, perr
}

// claudeArgs appends the request's option flags to base; shared by the one-shot and the live session.
func claudeArgs(args []string, req RunOptions) []string {
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
		args = append(args, personaArgs(req.Persona)...)
	}
	if !req.Tools {

		args = append(args, "--tools", "", "--setting-sources", "")
	} else {
		args = append(args, "--exclude-dynamic-system-prompt-sections") // per-session sections defeat the prompt cache
	}
	return args
}

// claudeCmd resolves the launch plan for args and returns the unstarted child with its env layered (--env last).
func claudeCmd(ctx context.Context, req RunOptions, args []string, prompt string) (*exec.Cmd, *TailBuffer, ClaudeLaunch, error) {
	if req.Launcher == nil {
		return nil, nil, ClaudeLaunch{}, errors.New("claude: no launcher (caller must set RunOptions.Launcher)")
	}
	o := ClaudeOptions{Provider: cmp.Or(req.Provider, sessionProvider("claude")), Model: req.Model, KeepEnv: req.KeepEnv}
	cl, err := req.Launcher.Claude(ctx, o, args)
	if err != nil {
		return nil, nil, cl, err
	}
	cmd, tail := childCmd(ctx, cl.Binary, cl.Argv, nil, req.WorkDir, prompt, req.KeepEnv, false)
	cmd.Env = scrubIsolated(ctx, cl.Env, append(slices.Clone(req.KeepEnv), slices.Collect(maps.Keys(cl.Set))...))
	cmd.Env = append(cmd.Env, req.Env...) // --env goes last, after the route keys, so it wins
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
func parseClaudeStream(br *bufio.Reader, sink Sink, logs ...eventLog) (res Result, _ error) {
	return parseClaudeEvents(br, false, sink, logs...)
}

// parseClaudeEvents stops at the first result when untilResult is set, preserving buffered lines for the next turn.
func parseClaudeEvents(br *bufio.Reader, untilResult bool, sink Sink, logs ...eventLog) (res Result, _ error) {
	lg := pickLog(logs)
	sawResult, wrote := false, false
	ts := transportTracker{start: clock()}
	defer func() { res.Transport = ts.result() }()
	err := decodeLines(br, lg, func(raw []byte) (bool, error) {
		var ev claudeEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return false, fmt.Errorf("decode stream-json: %w", err)
		}
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		switch ev.Type {
		case "stream_event":
			ts.event(clock())
		case "system":
			if ev.Subtype == "api_retry" {
				ts.retry(ev.ErrorStatus)
				lg.emit("retry", fmt.Sprintf("attempt %d/%d status=%d retry_in=%dms",
					ev.Attempt, ev.MaxRetries, ts.lastStatus(), ev.RetryDelayMs))
				if err := sink.Emit(Event{Type: EventRetry, Status: strconv.Itoa(ts.lastStatus()),
					Text: fmt.Sprintf("attempt %d/%d retry_in=%dms", ev.Attempt, ev.MaxRetries, ev.RetryDelayMs)}); err != nil {
					return false, err
				}
			}
			if ev.Model != "" {
				res.Model = ev.Model
			}
		case "assistant":
			if ev.ParentToolUseID != "" {
				return false, nil
			}
			msg := ev.object()
			if msg.Model != "" {
				res.Model = msg.Model
			}
			logClaudeAssistant(lg, msg)
			if err := emitClaudeAssistant(sink, msg, &wrote); err != nil {
				return false, err
			}
		case "result":
			sawResult = true
			if err := applyClaudeResult(&res, ev); err != nil {
				_ = emitError(lg, sink, err.Error())
				return false, err
			}
			lg.emit("result", ev.Result)
			if untilResult {
				return true, nil
			}
		case "error":
			if t := ev.text(); t != "" {
				_ = emitError(lg, sink, t)
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

func logClaudeAssistant(lg eventLog, msg claudeMessage) {
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			lg.emit("assistant", b.Text)
		case "tool_use":
			lg.emit("tool", b.Name)
		}
	}
}

func emitClaudeAssistant(sink Sink, msg claudeMessage, wrote *bool) error {
	for _, b := range msg.Content {
		var ev Event
		switch {
		case b.Type == "tool_use":
			ev = toolEvent(b.Name, b.ID)
		case b.Type == "text" && b.Text != "":
			ev = Event{Type: EventText, Text: b.Text, Sep: *wrote}
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

func applyClaudeResult(res *Result, ev claudeEvent) error {
	res.Answer = ev.Result
	u := ev.Usage
	res.Usage = Usage{InputTokens: u.InputTokens, CacheCreationTokens: u.CacheCreationInputTokens,
		CacheReadTokens: u.CacheReadInputTokens, OutputTokens: u.OutputTokens, CostUSD: ev.TotalCostUSD}
	if len(ev.Structured) > 0 && string(ev.Structured) != "null" {
		res.Structured = ev.Structured
	}
	if ev.IsError {
		return resultError{fmt.Errorf("claude %s: %s", ev.Subtype, ev.Result)}
	}
	return nil
}

func claudeAttach(prompt string, paths []string, tools bool) (string, error) {
	images, rest := splitImages(paths)
	if len(images) > 0 && !tools {
		return "", fmt.Errorf("--%w %s: image attach needs tools for claude", ErrBadAttach, images[0])
	}
	prompt, err := attachInline(prompt, rest)
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
