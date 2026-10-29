package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// AgyRequest is one headless agy run: the prompt rides stdin as a single stream-json NDJSON line.
// Env carries extra launch vars; agy authenticates with its own cached Google sign-in, never a key.
type AgyRequest struct {
	Prompt, Model, Effort, Cwd, SessionID string
	Tools, Autonomous                     bool
	DebugFile                             string // --log-file
	OutputSchema                          string // --json-schema
	Env, KeepEnv                          []string
}

// agyEffort maps the harness's own effort vocabulary onto agy's (identity today; unknown values drop).
var agyEffort = map[string]string{"low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max"}

// AgyDefaultModel runs agy when the caller names no model: fast, and enough for headless legs.
const AgyDefaultModel = "gemini-3.8-flash-high"

// agyToolLessPreamble is agy's own verified tool-less instruction (no tools-off flag exists).
const agyToolLessPreamble = "Answer from the text below only. Do not call any tools.\n\n"

// agyArgv builds agy's headless argv: stream-json both ways, the prompt rides stdin behind -p=.
func agyArgv(r AgyRequest) []string {
	argv := []string{"--model", r.Model}
	if e := agyEffort[r.Effort]; e != "" {
		argv = append(argv, "--effort", e)
	}
	if r.SessionID != "" { // resume a prior agy conversation rather than starting fresh
		argv = append(argv, "--conversation", r.SessionID)
	}
	argv = append(argv, "--input-format", "stream-json", "--output-format", "stream-json")
	if r.OutputSchema != "" {
		argv = append(argv, "--json-schema", r.OutputSchema)
	}
	if r.Tools {
		argv = append(argv, "--dangerously-skip-permissions")
	}
	if r.Autonomous {
		argv = append(argv, "--mode", "auto")
	}
	if r.DebugFile != "" {
		argv = append(argv, "--log-file", r.DebugFile)
	}
	return append(argv, "-p=")
}

// agyStdin is the one NDJSON line agy reads behind --input-format stream-json.
func agyStdin(r AgyRequest) (string, error) {
	content := r.Prompt
	if !r.Tools {
		content = agyToolLessPreamble + content
	}
	line, err := json.Marshal(map[string]any{"event": "user", "message": map[string]any{"content": content}})
	if err != nil {
		return "", fmt.Errorf("agy: encode stdin: %w", err)
	}
	return string(line) + "\n", nil
}

// RunAgy drives `agy` headless over stream-json: agent_response text deltas stream live, and the
// result event carries the final answer, usage, conversation id and status.
func RunAgy(ctx context.Context, r AgyRequest, sink Sink) (Result, error) {
	bin, err := paths.VendorBin("agy")
	if err != nil {
		return Result{}, fmt.Errorf("gemini: %w", err)
	}
	stdin, err := agyStdin(r)
	if err != nil {
		return Result{}, err
	}
	cmd, tail := childCmd(ctx, bin, agyArgv(r), r.Env, r.Cwd, stdin, r.KeepEnv, true)
	return runEventStream(ctx, "gemini", cmd, tail, sink, parseAgyStream, nil)
}

// agyEvent mirrors agy's stream-json schema: one JSON object per line.
type agyEvent struct {
	Event      string `json:"event"`
	ConvID     string `json:"conversation_id"` // init event only
	StepUpdate struct {
		StepType  string `json:"step_type"`
		TextDelta string `json:"text_delta"`
	} `json:"step_update"`
	Result struct {
		ConversationID string          `json:"conversation_id"`
		Status         string          `json:"status"`
		Response       string          `json:"response"`
		Structured     json.RawMessage `json:"structured_output"`
		Error          string          `json:"error"`
		Usage          struct {
			InputTokens     int     `json:"input_tokens"`
			OutputTokens    int     `json:"output_tokens"`
			ThinkingTokens  int     `json:"thinking_tokens"`
			CacheReadTokens int     `json:"cache_read_tokens"`
			CostUSD         float64 `json:"cost_usd"` // not reported by agy today; read if it ever is
		} `json:"usage"`
		DeniedActions []struct {
			Action      string `json:"action"`
			DisplayName string `json:"display_name"`
		} `json:"denied_actions"`
	} `json:"result"`
}

// parseAgyStream reads `agy --output-format stream-json`: agent_response deltas, then one result event.
// An optional eventLog gets the deltas as one assistant line per run of text, then the result or error.
// onMeaningful is unused here (agy carries no stall watchdog); it only matches runEventStream's parse type.
func parseAgyStream(r io.Reader, sink Sink, onMeaningful func(), logs ...eventLog) (res Result, _ error) {
	ts := transportTracker{start: clock()}
	defer func() { res.Transport = ts.timing() }()
	lg := pickLog(logs)
	var answer, pending strings.Builder
	flush := func() {
		lg.emit("assistant", pending.String())
		pending.Reset()
	}
	var response string
	var sawResult bool
	dec := json.NewDecoder(r)
	for {
		var ev agyEvent
		if err := dec.Decode(&ev); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			flush()
			lg.emit("error", "decode agy stream-json: "+err.Error())
			_ = sink.Emit(Event{Type: EventError, Text: "decode agy stream-json: " + err.Error()}) // the decode error aborts
			return res, fmt.Errorf("decode agy stream-json: %w", err)
		}
		ts.event(clock())
		switch ev.Event {
		case "init":
			res.SessionID = ev.ConvID
		case "step_update":
			if ev.StepUpdate.StepType != "agent_response" || ev.StepUpdate.TextDelta == "" {
				continue
			}
			if err := sink.Emit(Event{Type: EventText, Text: ev.StepUpdate.TextDelta}); err != nil {
				return res, err
			}
			answer.WriteString(ev.StepUpdate.TextDelta)
			pending.WriteString(ev.StepUpdate.TextDelta)
		case "result":
			sawResult = true
			response = ev.Result.Response
			flush()
			if err := applyAgyResult(&res, ev); err != nil {
				lg.emit("error", err.Error())
				_ = sink.Emit(Event{Type: EventError, Text: err.Error()}) // the agy error returned below aborts
				res.Answer = strings.TrimSpace(answer.String())
				return res, err
			}
			lg.emit("result", cmp.Or(response, answer.String()))
		}
	}
	flush()
	// A run whose text arrived only in result.response (no agent_response deltas) still answers.
	if answer.Len() == 0 && response != "" {
		if err := sink.Emit(Event{Type: EventText, Text: response}); err != nil {
			return res, err
		}
		answer.WriteString(response)
	}
	res.Answer = strings.TrimSpace(answer.String())
	if !sawResult {
		return res, errors.New("agy: stream ended without a result event")
	}
	return res, nil
}

func applyAgyResult(res *Result, ev agyEvent) error {
	out := ev.Result
	if out.ConversationID != "" {
		res.SessionID = out.ConversationID
	}
	res.Usage = Usage{
		InputTokens:     out.Usage.InputTokens,
		OutputTokens:    out.Usage.OutputTokens + out.Usage.ThinkingTokens,
		CacheReadTokens: out.Usage.CacheReadTokens,
		CostUSD:         out.Usage.CostUSD,
	}
	if len(out.Structured) > 0 && string(out.Structured) != "null" {
		res.Structured = out.Structured
	}
	if len(out.DeniedActions) > 0 {
		names := make([]string, len(out.DeniedActions))
		for i, d := range out.DeniedActions {
			names[i] = cmp.Or(d.DisplayName, d.Action)
		}
		fmt.Fprintf(os.Stderr, "agy: denied %s\n", strings.Join(names, ", "))
	}
	if out.Status != "SUCCESS" {
		return errors.New(cmp.Or(out.Error, "agy run "+cmp.Or(out.Status, "failed")))
	}
	return nil
}
