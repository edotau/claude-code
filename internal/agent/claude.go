package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/providers"
)

// claudeRunner execs `claude -p --output-format stream-json` with the launcher's provider env and overlay.
type claudeRunner struct{}

func (claudeRunner) Name() string { return "claude" }

func (claudeRunner) Caps() Caps {
	return Caps{Model: true, Provider: true, Effort: true, WorkDir: true, SessionID: true, Tools: true, NoTools: true, NoPersist: true, SafeMode: true, DebugFile: true}
}

func (claudeRunner) Available() error { _, err := launch.ClaudeBinary(); return err }

func (claudeRunner) Run(ctx context.Context, req Request, stream io.Writer) (Result, error) {
	if req.NoTools {
		if req.Tools {
			return Result{}, errors.New("claude: --no-tools and --tools are exclusive")
		}
		if res, done, err := claudeOneShot(ctx, req, stream); done {
			return res, err
		}
	}
	prompt, err := attachInline(req.Prompt, req.Attach)
	if err != nil {
		return Result{}, err
	}
	args := []string{"--print", "--output-format", "stream-json", "--verbose", "--include-partial-messages"}
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
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if !req.Tools {
		// A one-shot answer needs no house rules: CLAUDE.md + rules + skills + roster were ~395K cache-write tokens per call.
		args = append(args, "--tools", "", "--setting-sources", "")
	}
	plan, err := launch.Claude(ctx, launch.Options{Provider: cmp.Or(req.Provider, sessionProvider("claude")), Model: req.Model, KeepEnv: req.KeepEnv}, args)
	if err != nil {
		return Result{}, err
	}
	if err := plan.WriteOverlay(); err != nil {
		return Result{}, err
	}
	cmd, tail := childCmd(ctx, plan.Binary, plan.Argv, nil, req.WorkDir, prompt, req.KeepEnv, false)
	// plan.Env also merges the vault's custom env (any profile's vars): filter the result, sparing only the plan's own
	// route and token (plan.Set), so another profile's custom credential var never reaches this child
	cmd.Env = scrubIsolated(ctx, plan.Env(os.Environ()), append(slices.Clone(req.KeepEnv), slices.Collect(maps.Keys(plan.Set))...))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	res, perr := parseClaudeStream(stdout, stream, eventLogFor(ctx))
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	res.Provider = plan.Set[providers.PinProfile]
	if werr != nil || ctx.Err() != nil {
		return res, childErr(ctx, "claude", errors.Join(werr, perr), tail)
	}
	return res, perr
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

// parseClaudeStream streams top-level assistant text to w and returns the result event's envelope.
// A json.Decoder, not a line scanner: the init event routinely exceeds a scanner's 64KB line cap.
// An optional eventLog gets one formatted line per assistant text, tool call, result and error.
func parseClaudeStream(r io.Reader, w io.Writer, logs ...eventLog) (res Result, _ error) {
	lg := pickLog(logs)
	sawResult, wrote := false, false
	ts := transportTracker{start: clock()}
	defer func() { res.Transport = ts.result() }()
	dec := json.NewDecoder(r)
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
			var err error
			wrote, err = writeClaudeAssistant(w, msg, wrote)
			if err != nil {
				return res, err
			}
		case "result":
			sawResult = true
			if err := applyClaudeResult(&res, ev); err != nil {
				lg.emit("error", err.Error())
				return res, err
			}
			lg.emit("result", ev.Result)
		case "error":
			if t := ev.text(); t != "" {
				lg.emit("error", t)
				return res, fmt.Errorf("claude: %s", t)
			}
		}
	}
	if !sawResult {
		return res, errors.New("claude: stream ended without a result event")
	}
	return res, nil
}

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

func writeClaudeAssistant(w io.Writer, msg claudeMessage, wrote bool) (bool, error) {
	for _, b := range msg.Content {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		if wrote {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return wrote, err
			}
		}
		if _, err := io.WriteString(w, b.Text); err != nil {
			return wrote, err
		}
		wrote = true
	}
	return wrote, nil
}

func applyClaudeResult(res *Result, ev claudeEvent) error {
	res.Answer = ev.Result
	u := ev.Usage
	res.Usage = Usage{InputTokens: u.InputTokens, CacheCreationTokens: u.CacheCreationInputTokens,
		CacheReadTokens: u.CacheReadInputTokens, OutputTokens: u.OutputTokens}
	if ev.IsError {
		return fmt.Errorf("claude %s: %s", ev.Subtype, ev.Result)
	}
	return nil
}
