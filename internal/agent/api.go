package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
func claudeOneShot(ctx context.Context, req Request, sink Sink) (res Result, ok bool, err error) {
	_, t, err := resolveTarget(req)
	if err != nil {
		return Result{}, true, err
	}
	p := t.Provider
	res = Result{Provider: p.Name, Model: t.Model}
	if p.Auth.Method() == providers.AuthCLI {
		return res, false, nil
	}
	if len(req.Env) > 0 {
		res.Dropped = []string{"--env"} // the HTTP one-shot spawns no process to carry it
	}
	prompt, err := claudeAttach(req.Prompt, req.Attach, false)
	if err != nil {
		return Result{}, true, err
	}
	var answer strings.Builder
	w := io.MultiWriter(sinkWriter(sink), &answer)
	if base, ok := providers.AnthropicRoute(p, t.Model); ok {
		out, err := anthropicsdk.Stream(ctx, anthropicsdk.Request{
			BaseURL: base, Model: t.Model, Prompt: prompt, Effort: req.Effort, MaxTokens: apiMaxTokens,
			Authorize: func(ctx context.Context, h http.Header) error { return providers.Authorize(ctx, p, h) },
		}, w)
		if out.Model != "" {
			res.Model = out.Model
		}
		res.Answer, res.Usage = answer.String(), Usage{InputTokens: int(out.InputTokens), OutputTokens: int(out.OutputTokens)}
		logAPIRun(ctx, res, err)
		return res, true, err
	}
	if err := providers.ClaudeCodeError(p, t.Model); err != nil { // Responses-only: the one-shot has no such wire
		return res, true, &Error{Kind: KindFatal, Agent: "claude", Err: err}
	}
	if _, servable, why := providers.WireFor(p, t.Model); !servable {
		return res, true, &Error{Kind: KindFatal, Agent: "claude", Err: errors.New(why)}
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
func logAPIRun(ctx context.Context, res Result, err error) {
	lg := eventLogFor(ctx)
	if res.Answer != "" {
		lg.emit("assistant", res.Answer)
	}
	if err != nil {
		lg.emit("error", err.Error())
		return
	}
	lg.emit("result", fmt.Sprintf("tokens in=%d out=%d", res.Usage.InputTokens, res.Usage.OutputTokens))
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
func chatStream(ctx context.Context, base string, p *providers.Provider, model, prompt, effort string, w io.Writer) (Usage, string, error) {
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
		return Usage{}, "", err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if err := providers.Authorize(ctx, p, hreq.Header); err != nil {
		return Usage{}, "", err
	}
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return Usage{}, "", ctx.Err()
		}
		return Usage{}, "", &httpError{msg: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
		after, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return Usage{}, "", &httpError{status: resp.StatusCode, after: time.Duration(after) * time.Second,
			msg: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))}
	}
	return parseChatStream(resp.Body, w)
}

func parseChatStream(r io.Reader, w io.Writer) (Usage, string, error) {
	var usage Usage
	var served string
	sawDone := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		data, ok := bytes.CutPrefix(sc.Bytes(), []byte("data:"))
		if data = bytes.TrimSpace(data); !ok || len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			sawDone = true
			break
		}
		var chunk struct {
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
		if err := json.Unmarshal(data, &chunk); err != nil {
			return usage, served, fmt.Errorf("decode chat chunk: %w", err)
		}
		if chunk.Error != nil {
			return usage, served, fmt.Errorf("upstream error mid-stream: %s", *chunk.Error)
		}
		if chunk.Model != "" {
			served = chunk.Model
		}
		if chunk.Usage != nil {
			usage = Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				if _, err := io.WriteString(w, c.Delta.Content); err != nil {
					return usage, served, err
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return usage, served, err
	}
	if !sawDone {
		return usage, served, fmt.Errorf("chat stream ended without [DONE]: %w", io.ErrUnexpectedEOF) // a dropped stream retries
	}
	return usage, served, nil
}
