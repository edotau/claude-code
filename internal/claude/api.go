package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/agent"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/anthropicsdk"
	"github.com/edotau/claude-code/internal/providers"
)

// apiMaxTokens caps a one-shot answer; reasoning models spend part of it thinking.
const apiMaxTokens = 16_000

// claudeOneShot is claude's tool-less one-shot via the anthropic SDK or openai route; ok is false for cli auth.
func claudeOneShot(ctx context.Context, req agent.RunOptions, sink agent.Sink) (res agent.Result, ok bool, err error) {
	_, t, err := agent.ResolveRequestTarget(req)
	if err != nil {
		return agent.Result{}, true, err
	}
	p := t.Provider
	res = agent.Result{Provider: p.Name, Model: t.Model}
	if p.Auth.Method() == providers.AuthCLI {
		return res, false, nil
	}
	if len(req.Env) > 0 {
		res.Dropped = []string{"--env"} // the HTTP one-shot spawns no process to carry it
	}
	prompt, err := claudeAttach(req.Prompt, req.Attach, false)
	if err != nil {
		return agent.Result{}, true, err
	}
	var answer strings.Builder
	w := io.MultiWriter(agent.SinkWriter(sink), &answer)
	if base, ok := providers.AnthropicRoute(p, t.Model); ok {
		out, err := anthropicsdk.Stream(ctx, anthropicsdk.Request{
			BaseURL: base, Model: t.Model, Prompt: prompt, Effort: req.Effort, MaxTokens: apiMaxTokens,
			Authorize: func(ctx context.Context, h http.Header) error { return providers.Authorize(ctx, p, h) },
		}, w)
		if out.Model != "" {
			res.Model = out.Model
		}
		res.Answer, res.Usage = answer.String(), agent.Usage{InputTokens: int(out.InputTokens), OutputTokens: int(out.OutputTokens)}
		logAPIRun(ctx, res, err)
		return res, true, err
	}
	if err := providers.ClaudeCodeError(p, t.Model); err != nil { // Responses-only: the one-shot has no such wire
		return res, true, &agent.Error{Kind: agent.KindFatal, Agent: "claude", Err: err}
	}
	if _, servable, why := providers.WireFor(p, t.Model); !servable {
		return res, true, &agent.Error{Kind: agent.KindFatal, Agent: "claude", Err: errors.New(why)}
	}
	base, _ := p.Route(providers.ProtocolOpenAI)
	usage, model, err := chatStream(ctx, base, p, t.Model, prompt, req.Effort, w)
	if model != "" {
		res.Model = model
	}
	res.Answer, res.Usage = answer.String(), usage
	logAPIRun(ctx, res, err)
	return res, true, err
}

// logAPIRun writes the one-shot's answer, usage or error to the dispatch log (the stream has no event boundaries).
func logAPIRun(ctx context.Context, res agent.Result, err error) {
	lg := agent.EventLogFor(ctx)
	if res.Answer != "" {
		lg.Emit("assistant", res.Answer)
	}
	if err != nil {
		lg.Emit("error", err.Error())
		return
	}
	lg.Emit("result", fmt.Sprintf("tokens in=%d out=%d", res.Usage.InputTokens, res.Usage.OutputTokens))
}

// httpError carries an upstream status and Retry-After into Classify.
type httpError struct {
	status int
	after  time.Duration
	msg    string
}

func (e *httpError) Error() string             { return e.msg }
func (e *httpError) StatusCode() int           { return e.status }
func (e *httpError) RetryAfter() time.Duration { return e.after }

// chatStream POSTs {base}/chat/completions with stream=true and writes content deltas to w.
func chatStream(ctx context.Context, base string, p *providers.Provider, model, prompt, effort string, w io.Writer) (agent.Usage, string, error) {
	body := map[string]any{
		"model":          model,
		"messages":       []map[string]string{{"role": "user", "content": prompt}},
		"stream":         true,
		"stream_options": map[string]bool{"include_usage": true},
	}
	if effort != "" {
		body["reasoning_effort"] = effort
	}
	payload, _ := json.Marshal(body)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return agent.Usage{}, "", err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if err := providers.Authorize(ctx, p, hreq.Header); err != nil {
		return agent.Usage{}, "", err
	}
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return agent.Usage{}, "", ctx.Err()
		}
		return agent.Usage{}, "", &httpError{msg: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
		after, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return agent.Usage{}, "", &httpError{status: resp.StatusCode, after: time.Duration(after) * time.Second,
			msg: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))}
	}
	return parseChatStream(resp.Body, w)
}

// chatChunk is one data: payload of a chat-completions stream.
type chatChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *json.RawMessage `json:"error"`
}

// applyChatLine folds one stream line into usage, served and w; done reports the [DONE] marker.
func applyChatLine(line []byte, w io.Writer, usage *agent.Usage, served *string) (done bool, _ error) {
	data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("data:"))
	if data = bytes.TrimSpace(data); !ok || len(data) == 0 {
		return false, nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		return true, nil
	}
	var chunk chatChunk
	if json.Unmarshal(data, &chunk) != nil {
		return false, nil // skip an undecodable data: line
	}
	if chunk.Error != nil {
		return false, fmt.Errorf("upstream error mid-stream: %s", *chunk.Error)
	}
	if chunk.Model != "" {
		*served = chunk.Model
	}
	if chunk.Usage != nil {
		*usage = agent.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
	}
	for _, c := range chunk.Choices {
		if c.Delta.Content != "" {
			if _, werr := io.WriteString(w, c.Delta.Content); werr != nil {
				return false, werr
			}
		}
	}
	return false, nil
}

func parseChatStream(r io.Reader, w io.Writer) (agent.Usage, string, error) {
	var usage agent.Usage
	var served string
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if done, lerr := applyChatLine(line, w, &usage, &served); done || lerr != nil {
			return usage, served, lerr
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return usage, served, err
		}
	}
	return usage, served, fmt.Errorf("chat stream ended without [DONE]: %w", io.ErrUnexpectedEOF) // a dropped stream retries
}
