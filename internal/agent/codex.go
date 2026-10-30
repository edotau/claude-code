package agent

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
)

// codexHasJSON is true when the installed codex has `exec --json`; checked once per process.
var codexHasJSON = sync.OnceValue(func() bool {
	bin, err := paths.VendorBin("codex")
	if err != nil {
		return false
	}
	out, _ := exec.Command(bin, "exec", "--help").CombinedOutput()
	return strings.Contains(string(out), "--json")
})

// codexExecArgs inserts extra right after the exec subcommand, where codex takes its flags.
func codexExecArgs(argv []string, extra ...string) []string {
	if len(extra) == 0 || len(argv) == 0 || argv[0] != "exec" {
		return argv
	}
	return append(append([]string{"exec"}, extra...), argv[1:]...)
}

// codexSchemaFile writes the strict form of schema to a 0600 temp file for --output-schema; remove it after the run.
func codexSchemaFile(schema string) (string, error) {
	var v any
	if err := json.Unmarshal([]byte(schema), &v); err != nil {
		return "", fmt.Errorf("--output-schema: %w", err)
	}
	body, _ := json.Marshal(codexStrict(v))
	f, err := os.CreateTemp("", "codex-schema-*.json") // CreateTemp is 0600
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// codexStrict makes every object schema OpenAI-strict (additionalProperties false, all properties required);
// the harness still validates the answer against the caller's original schema.
func codexStrict(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range codexSchemaMaps {
			if m, ok := x[k].(map[string]any); ok {
				for n, c := range m {
					m[n] = codexStrict(c)
				}
			}
		}
		for _, k := range codexSubschemas {
			if c, ok := x[k]; ok {
				x[k] = codexStrict(c)
			}
		}
		if props, ok := x["properties"].(map[string]any); ok {
			req := make([]string, 0, len(props))
			for k := range props {
				req = append(req, k)
			}
			sort.Strings(req)
			x["required"], x["additionalProperties"] = req, false
		}
	case []any:
		for i, c := range x {
			x[i] = codexStrict(c)
		}
	}
	return v
}

// codexSchemaMaps hold name-to-schema maps; codexSubschemas hold one schema or a list of them. Other keys are data.
var (
	codexSchemaMaps = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}
	codexSubschemas = []string{"items", "prefixItems", "additionalProperties", "unevaluatedProperties", "contains",
		"not", "if", "then", "else", "anyOf", "oneOf", "allOf"}
)

// codexEvent mirrors `codex exec --json`: thread.started, item.started|completed, turn.completed|failed, error.
type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     struct {
		ID      string `json:"id"`
		Type    string `json:"type"` // agent_message | command_execution | error | reasoning | ...
		Text    string `json:"text"`
		Command string `json:"command"`
		Status  string `json:"status"`
	} `json:"item"`
	Usage struct {
		InputTokens       int `json:"input_tokens"`
		CachedInputTokens int `json:"cached_input_tokens"`
		OutputTokens      int `json:"output_tokens"`
	} `json:"usage"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// parseCodexStream reads `codex exec --json`: agent messages become text events (the last is the answer),
// command executions become tool events, turn.completed adds usage and turn.failed fails the run.
func parseCodexStream(r io.Reader, sink Sink, onMeaningful func(), logs ...eventLog) (res Result, _ error) {
	ts := transportTracker{start: clock()}
	defer func() { res.Transport = ts.timing() }()
	lg := pickLog(logs)
	var last string
	var failure string
	texts := 0
	dec := json.NewDecoder(r)
	for {
		var ev codexEvent
		if err := dec.Decode(&ev); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return res, fmt.Errorf("decode codex json: %w", err)
		}
		ts.event(clock())
		it := ev.Item
		switch {
		case ev.Type == "thread.started":
			res.SessionID = ev.ThreadID
		case ev.Type == "item.completed" && it.Type == "agent_message" && it.Text != "":
			if onMeaningful != nil {
				onMeaningful()
			}
			if err := sink.Emit(Event{Type: EventText, Text: it.Text, Sep: texts > 0}); err != nil {
				return res, err
			}
			texts++
			last = it.Text
			lg.emit("assistant", it.Text)
		case ev.Type == "item.started" && it.Type == "command_execution":
			tool := toolEvent("shell", it.ID)
			tool.Text = it.Command
			if err := sink.Emit(tool); err != nil {
				return res, err
			}
			lg.emit("tool", it.Command)
		case ev.Type == "item.completed" && it.Type == "command_execution":
			if err := sink.Emit(Event{Type: EventToolResult, Tool: &ToolRef{Name: "shell", ID: it.ID}, Status: it.Status}); err != nil {
				return res, err
			}
		case ev.Type == "turn.completed":
			u := ev.Usage
			res.Usage.InputTokens += u.InputTokens - u.CachedInputTokens
			res.Usage.CacheReadTokens += u.CachedInputTokens
			res.Usage.OutputTokens += u.OutputTokens
			lg.emit("result", fmt.Sprintf("turn finished, tokens in=%d out=%d", u.InputTokens, u.OutputTokens))
		case ev.Type == "turn.failed":
			failure = cmp.Or(ev.Error.Message, "codex turn failed")
			lg.emit("error", failure)
			if err := sink.Emit(Event{Type: EventError, Text: failure}); err != nil {
				return res, err
			}
		}
	}
	res.Answer = strings.TrimSpace(last)
	if failure != "" {
		return res, errors.New(failure)
	}
	return res, nil
}
