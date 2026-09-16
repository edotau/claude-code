package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

// apiRunner is a tool-less one-shot: the anthropic route via the SDK, else the openai chat-completions route.
type apiRunner struct{}

func (apiRunner) Name() string     { return "api" }
func (apiRunner) Caps() Caps       { return Caps{Model: true, Provider: true, Effort: true} }
func (apiRunner) Available() error { _, err := providers.Load(); return err }

func (apiRunner) Run(ctx context.Context, req Request, stream io.Writer) (Result, error) {
	_, t, err := resolveTarget(req)
	if err != nil {
		return Result{}, err
	}
	p := t.Provider
	res := Result{Provider: p.Name, Model: t.Model}
	if p.Auth.Type == providers.AuthPassthrough {
		return res, &Error{Kind: KindFatal, Agent: "api", Err: fmt.Errorf("provider %s uses passthrough (subscription) auth; the api runner needs a key (try --agent claude)", p.Name)}
	}
	var answer strings.Builder
	w := io.MultiWriter(stream, &answer)
	if base, ok := p.Route(providers.DialectAnthropic); ok {
		out, err := anthropicsdk.Stream(ctx, anthropicsdk.Request{
			BaseURL: base, Model: t.Model, Prompt: req.Prompt, Effort: req.Effort, MaxTokens: apiMaxTokens,
			Authorize: func(ctx context.Context, h http.Header) error { return providers.Authorize(ctx, p, h) },
		}, w)
		if out.Model != "" {
			res.Model = out.Model
		}
		res.Answer, res.Usage = answer.String(), Usage{int(out.InputTokens), int(out.OutputTokens)}
		return res, err
	}
	base, ok := p.Route(providers.DialectOpenAI)
	if !ok {
		return res, &Error{Kind: KindFatal, Agent: "api", Err: fmt.Errorf("provider %s has neither an anthropic nor an openai route", p.Name)}
	}
	usage, model, err := chatStream(ctx, base, p, t.Model, req.Prompt, req.Effort, w)
	if model != "" {
		res.Model = model
	}
	res.Answer, res.Usage = answer.String(), usage
	return res, err
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
	var usage Usage
	var served string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if data = strings.TrimSpace(data); !ok || data == "" {
			continue
		}
		if data == "[DONE]" {
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
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return usage, served, fmt.Errorf("decode chat chunk: %w", err)
		}
		if chunk.Error != nil {
			return usage, served, fmt.Errorf("upstream error mid-stream: %s", *chunk.Error)
		}
		if chunk.Model != "" {
			served = chunk.Model
		}
		if chunk.Usage != nil {
			usage = Usage{chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens}
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
	return usage, served, nil
}
