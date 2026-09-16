package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/providers"
)

// claudeRunner execs `claude -p --output-format stream-json` with the launcher's provider env and overlay.
type claudeRunner struct{}

func (claudeRunner) Name() string { return "claude" }

func (claudeRunner) Caps() Caps {
	return Caps{Model: true, Provider: true, Effort: true, WorkDir: true, SessionID: true, Tools: true}
}

func (claudeRunner) Available() error { _, err := launch.ClaudeBinary(); return err }

func (claudeRunner) Run(ctx context.Context, req Request, stream io.Writer) (Result, error) {
	args := []string{"--print", "--output-format", "stream-json", "--verbose"}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if !req.Tools {
		args = append(args, "--tools", "")
	}
	plan, err := launch.Claude(ctx, launch.Options{Provider: req.Provider, Model: req.Model}, args)
	if err != nil {
		return Result{}, err
	}
	if err := plan.WriteOverlay(); err != nil {
		return Result{}, err
	}
	cmd, tail := childCmd(ctx, plan.Binary, plan.Argv, nil, req.WorkDir, req.Prompt)
	cmd.Env = plan.Env(os.Environ())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	res, perr := parseClaudeStream(stdout, stream)
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	res.Provider = plan.Set[providers.PinProvider]
	if werr != nil || ctx.Err() != nil {
		return res, childErr(ctx, "claude", errors.Join(werr, perr), tail)
	}
	return res, perr
}

// claudeEvent is the union of stream-json fields the runner reads.
type claudeEvent struct {
	Type            string `json:"type"`
	Subtype         string `json:"subtype"`
	SessionID       string `json:"session_id"`
	Model           string `json:"model"`
	ParentToolUseID string `json:"parent_tool_use_id"`
	Message         struct {
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Usage   struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// parseClaudeStream streams top-level assistant text to w and returns the result event's envelope.
// A json.Decoder, not a line scanner: the init event routinely exceeds a scanner's 64KB line cap.
func parseClaudeStream(r io.Reader, w io.Writer) (Result, error) {
	var res Result
	sawResult, wrote := false, false
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
		case "system":
			if ev.Model != "" {
				res.Model = ev.Model
			}
		case "assistant":
			if ev.ParentToolUseID != "" {
				continue // a subagent's turn, not the answer
			}
			if ev.Message.Model != "" {
				res.Model = ev.Message.Model
			}
			for _, b := range ev.Message.Content {
				if b.Type != "text" || b.Text == "" {
					continue
				}
				if wrote {
					io.WriteString(w, "\n")
				}
				if _, err := io.WriteString(w, b.Text); err != nil {
					return res, err
				}
				wrote = true
			}
		case "result":
			sawResult = true
			res.Answer = ev.Result
			u := ev.Usage
			res.Usage = Usage{InputTokens: u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens, OutputTokens: u.OutputTokens}
			if ev.IsError {
				return res, fmt.Errorf("claude %s: %s", ev.Subtype, ev.Result)
			}
		}
	}
	if !sawResult {
		return res, errors.New("claude: stream ended without a result event")
	}
	return res, nil
}
