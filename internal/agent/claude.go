package agent

import (
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
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// claudeRunner execs `claude -p --output-format stream-json` with the launcher's provider env and overlay.
type claudeRunner struct{}

func (claudeRunner) Name() string { return "claude" }

func (claudeRunner) Caps() Caps {
	s, _ := LookupSchema("claude")
	return s.CLI.Caps
}

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
	res, perr := parseClaudeStream(stdout, sink, eventLogFor(ctx))
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	res.Provider = cl.Set[providers.PinProfile]
	if werr != nil || ctx.Err() != nil {
		return res, childErr(ctx, "claude", errors.Join(werr, perr), tail)
	}
	return res, perr
}

// claudeArgs appends the request's option flags to base; shared by the one-shot and the live session.
func claudeArgs(args []string, req RunOptions) []string {
	if req.NoPersist {
		args = append(args, "--no-session-persistence")
	}
	if req.SafeMode {
		args = append(args, "--safe-mode")
	}
	if req.DebugFile != "" {
		args = append(args, "--debug-file", req.DebugFile)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if req.SystemPrompt != "" {
		args = append(args, "--system-prompt", req.SystemPrompt)
	}
	if req.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.AppendSystemPrompt)
	}
	if req.Persona != nil {
		args = append(args, personaArgs(req.Persona)...)
	}
	if req.OutputSchema != "" {
		args = append(args, "--json-schema", req.OutputSchema)
	}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if !req.Tools {
		// A one-shot answer needs no house rules: CLAUDE.md + rules + skills + roster were ~395K cache-write tokens per call.
		args = append(args, "--tools", "", "--setting-sources", "")
	}
	return args
}

// claudeCmd resolves the launch plan for args and returns the unstarted child with its env layered (--env last).
func claudeCmd(ctx context.Context, req RunOptions, args []string, prompt string) (*exec.Cmd, *TailBuffer, ClaudeLaunch, error) {
	if req.Launcher == nil {
		return nil, nil, ClaudeLaunch{}, errors.New("claude: no launcher (caller must set Request.Launcher)")
	}
	o := ClaudeOptions{Provider: cmp.Or(req.Provider, sessionProvider("claude")), Model: req.Model, KeepEnv: req.KeepEnv}
	cl, err := req.Launcher.Claude(ctx, o, args)
	if err != nil {
		return nil, nil, cl, err
	}
	cmd, tail := childCmd(ctx, cl.Binary, cl.Argv, nil, req.WorkDir, prompt, req.KeepEnv, false)
	// cl.Env also merges the vault's custom env (any profile's vars): filter the result, sparing only the plan's own
	// route and token (cl.Set), so another profile's custom credential var never reaches this child
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
		Name string `json:"name"` // tool_use blocks
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
// A json.Decoder, not a line scanner: the init event routinely exceeds a scanner's 64KB line cap.
// An optional eventLog gets one formatted line per assistant text, tool call, result and error.
func parseClaudeStream(r io.Reader, sink Sink, logs ...eventLog) (res Result, _ error) {
	return parseClaudeEvents(json.NewDecoder(r), false, sink, logs...)
}

// parseClaudeEvents is parseClaudeStream over a caller-owned decoder; untilResult returns after the first result
// event, leaving the decoder positioned for the next turn of a live session.
func parseClaudeEvents(dec *json.Decoder, untilResult bool, sink Sink, logs ...eventLog) (res Result, _ error) {
	lg := pickLog(logs)
	sawResult, wrote := false, false
	ts := transportTracker{start: clock()}
	defer func() { res.Transport = ts.result() }()
	for {
		var ev claudeEvent
		if err := dec.Decode(&ev); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return res, fmt.Errorf("decode stream-json: %w", err)
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
					return res, err
				}
			}
			if ev.Model != "" {
				res.Model = ev.Model
			}
		case "assistant":
			if ev.ParentToolUseID != "" {
				continue // a subagent's turn, not the answer
			}
			msg := ev.object()
			if msg.Model != "" {
				res.Model = msg.Model
			}
			logClaudeAssistant(lg, msg)
			if err := emitClaudeAssistant(sink, msg, &wrote); err != nil {
				return res, err
			}
		case "result":
			sawResult = true
			if err := applyClaudeResult(&res, ev); err != nil {
				lg.emit("error", err.Error())
				_ = sink.Emit(Event{Type: EventError, Text: err.Error()}) // the parse error below already aborts
				return res, err
			}
			lg.emit("result", ev.Result)
			if untilResult {
				return res, nil
			}
		case "error":
			if t := ev.text(); t != "" {
				lg.emit("error", t)
				_ = sink.Emit(Event{Type: EventError, Text: t}) // the claude error below already aborts
				return res, fmt.Errorf("claude: %s", t)
			}
		}
	}
	if !sawResult {
		return res, errNoResult
	}
	return res, nil
}

var errNoResult = errors.New("claude: stream ended without a result event")

// clock is the time seam for stream timing.
var clock = time.Now

// transportTracker accumulates Transport stats while a claude stream is parsed.
type transportTracker struct {
	start, last time.Time
	statuses    []int
	events      int
	first, gap  time.Duration
}

func (t *transportTracker) event(now time.Time) {
	if t.events == 0 {
		t.first = now.Sub(t.start)
	} else if g := now.Sub(t.last); g > t.gap {
		t.gap = g
	}
	t.events++
	t.last = now
}

func (t *transportTracker) retry(status *int) {
	s := 0
	if status != nil {
		s = *status
	}
	t.statuses = append(t.statuses, s)
}

func (t *transportTracker) lastStatus() int { return t.statuses[len(t.statuses)-1] }

// result is nil when the stream showed neither a stream_event nor a retry.
func (t *transportTracker) result() *Transport {
	if t.events == 0 && len(t.statuses) == 0 {
		return nil
	}
	return &Transport{Retries: len(t.statuses), RetryStatuses: t.statuses,
		FirstEventMs: t.first.Milliseconds(), MaxGapMs: t.gap.Milliseconds()}
}

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

// emitClaudeAssistant emits msg's tool calls and text blocks; *wrote marks the first text, later text starts a turn.
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

// resultError is a failed turn whose result event was consumed, so the stream stays at a turn boundary.
type resultError struct{ error }

func (e resultError) Unwrap() error { return e.error }

// claudeAttach inlines text files; images are named by absolute path so claude's Read tool opens them (needs tools).
func claudeAttach(prompt string, paths []string, tools bool) (string, error) {
	images, rest := splitImages(paths)
	if len(images) > 0 && !tools {
		return "", fmt.Errorf("--%w %s: image attach needs tools for claude", ErrBadAttach, images[0])
	}
	prompt, err := attachInline(prompt, rest)
	if err != nil {
		return "", err
	}
	var promptSb361 strings.Builder
	for _, p := range images {
		promptSb361.WriteString("\n\nAttached image (open it with the Read tool): " + p)
	}
	prompt += promptSb361.String()
	return prompt, nil
}
