// In-process streaming HTTP one-shot for --tools=false asks on OpenAI, Responses, and Anthropic routes.
package agent

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/egress"
	"github.com/edotau/claude-code/internal/httpx"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/serving"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	oneShotMaxTokens = 16_000
	oneShotErrCap    = 4 << 10
)

var oneShotClient = httpx.NewClient(0, func() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 2 * time.Minute
	t.MaxIdleConnsPerHost = 16
	t.Proxy = egress.Proxy
	return t
}(), httpx.SameOriginRedirect)

// CanOneShotHTTP reports whether req on (p, model) can stream directly over HTTP without a child CLI.
func CanOneShotHTTP(p *providers.Provider, model string, req RunOptions) bool {
	if p == nil || model == "" || p.Auth.Method() == providers.AuthCLI || !oneShotFlagsOK(req) {
		return false
	}
	_, _, ok := oneShotEndpoint(p, model)
	return ok
}

func oneShotFlagsOK(req RunOptions) bool {
	if req.Tools || req.EnableMemory || req.Fleet || req.Persona != nil || req.Pack != nil {
		return false
	}
	return cmp.Or(req.SessionID, req.DebugFile, req.OutputSchema) == "" &&
		len(req.Env)+len(req.Attach)+len(req.GoogleTools)+len(req.Agents) == 0
}

// oneShotEndpoint selects the best streaming HTTP dialect and URL for (p, model).
func oneShotEndpoint(p *providers.Provider, model string) (dialect, url string, ok bool) {
	if base, has := providers.AnthropicRoute(p, model); has {
		return providers.ProtocolAnthropic, serving.EndpointURL(base, providers.ProtocolAnthropic, model, false), true
	}
	if base, has := p.Route(providers.ProtocolResponses); has && (p.Protocol == providers.ProtocolResponses || providers.ServesResponses(p, model)) {
		return providers.ProtocolResponses, strings.TrimRight(base, "/") + "/responses", true
	}
	if base, has := p.Route(providers.ProtocolOpenAI); has {
		return providers.ProtocolOpenAI, strings.TrimRight(base, "/") + "/chat/completions", true
	}
	return "", "", false
}

// OneShotHTTP executes a tool-less single-turn prompt over streaming HTTP, returning a standard wire.Result.
func OneShotHTTP(ctx context.Context, t providers.Target, req RunOptions, sink wire.Sink) (wire.Result, error) {
	dialect, url, ok := oneShotEndpoint(t.Provider, t.Model)
	if !ok {
		return wire.Result{}, errors.New("no supported HTTP one-shot dialect")
	}
	payload, err := oneShotBody(dialect, t.Model, req)
	if err != nil {
		return wire.Result{}, err
	}
	resp, err := sendOneShot(ctx, t.Provider, dialect, url, payload)
	if err != nil {
		return wire.Result{Provider: t.Provider.Name, Model: t.Model}, err
	}
	defer resp.Body.Close()
	var answer strings.Builder
	w := io.MultiWriter(wire.SinkWriter(sink), &answer)
	usage, served, err := parseOneShotStream(dialect, resp.Body, w)
	res := wire.Result{Provider: t.Provider.Name, Model: cmp.Or(served, t.Model), Answer: answer.String(), Usage: usage}
	if served != "" {
		res.Reported = strings.TrimSpace(served)
	}
	return res, err
}

func oneShotBody(dialect, model string, req RunOptions) ([]byte, error) {
	sys := strings.TrimSpace(strings.Join([]string{req.SystemPrompt, req.AppendSystemPrompt}, "\n"))
	switch dialect {
	case providers.ProtocolAnthropic:
		body := map[string]any{
			"model": model, "max_tokens": oneShotMaxTokens, "stream": true,
			"messages": []map[string]string{{"role": "user", "content": req.Prompt}},
		}
		if sys != "" {
			body["system"] = sys
		}
		return json.Marshal(body)
	case providers.ProtocolResponses:
		body := map[string]any{"model": model, "input": req.Prompt, "stream": true}
		if sys != "" {
			body["instructions"] = sys
		}
		if req.Effort != "" {
			body["reasoning"] = map[string]string{"effort": req.Effort}
		}
		return json.Marshal(body)
	default:
		msgs := make([]map[string]string, 0, 2)
		if sys != "" {
			msgs = append(msgs, map[string]string{"role": "system", "content": sys})
		}
		msgs = append(msgs, map[string]string{"role": "user", "content": req.Prompt})
		body := map[string]any{"model": model, "messages": msgs, "stream": true}
		if !strings.Contains(strings.ToLower(model), "gemini") {
			body["stream_options"] = map[string]bool{"include_usage": true}
			if req.Effort != "" {
				body["reasoning_effort"] = req.Effort
			}
		}
		return json.Marshal(body)
	}
}

type oneShotHTTPErr struct {
	status int
	after  time.Duration
	hint   retry.ShouldRetry
	msg    string
}

func (e *oneShotHTTPErr) Error() string                  { return e.msg }
func (e *oneShotHTTPErr) StatusCode() int                { return e.status }
func (e *oneShotHTTPErr) RetryAfter() time.Duration      { return e.after }
func (e *oneShotHTTPErr) ShouldRetry() retry.ShouldRetry { return e.hint }

func sendOneShot(ctx context.Context, p *providers.Provider, dialect, url string, payload []byte) (*http.Response, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if dialect == providers.ProtocolAnthropic {
		hreq.Header.Set("anthropic-version", "2023-06-01")
	}
	if err := providers.Authorize(ctx, p, hreq.Header); err != nil {
		return nil, err
	}
	resp, err := oneShotClient.Do(hreq)
	if err != nil {
		if h := egress.HostContext(ctx); h != "" {
			egress.Invalidate(h)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &oneShotHTTPErr{msg: err.Error()}
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, oneShotErrCap))
	after, _ := textutil.HeaderRetryAfter(resp.Header, time.Now())
	return nil, &oneShotHTTPErr{
		status: resp.StatusCode, after: max(after, 0),
		hint: retry.ParseShouldRetry(resp.Header.Get("X-Should-Retry")),
		msg:  fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 600)]))),
	}
}

func parseOneShotStream(dialect string, r io.Reader, w io.Writer) (wire.Usage, string, error) {
	var usage wire.Usage
	var served string
	var done bool
	br := bufio.NewReader(r)
	for {
		line, readErr := br.ReadBytes('\n')
		data, ok := sseDataLine(line)
		if ok {
			stop, err := applyOneShotData(dialect, data, w, &usage, &served, &done)
			if err != nil {
				return usage, served, err
			}
			if stop {
				return usage, served, nil
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return usage, served, readErr
		}
	}
	if done {
		return usage, served, nil
	}
	return usage, served, fmt.Errorf("one-shot stream ended before terminal event: %w", io.ErrUnexpectedEOF)
}

func sseDataLine(line []byte) ([]byte, bool) {
	data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("data:"))
	data = bytes.TrimSpace(data)
	return data, ok && len(data) > 0
}

func applyOneShotData(dialect string, data []byte, w io.Writer, usage *wire.Usage, served *string, done *bool) (bool, error) {
	if bytes.Equal(data, []byte("[DONE]")) {
		*done = true
		return true, nil
	}
	switch dialect {
	case providers.ProtocolAnthropic:
		return applyAnthropicSSE(data, w, usage, served, done)
	case providers.ProtocolResponses:
		return applyResponsesSSE(data, w, usage, served, done)
	default:
		return applyChatSSE(data, w, usage, served, done)
	}
}

func nonNullJSON(raw *json.RawMessage) []byte {
	if raw == nil {
		return nil
	}
	trimmed := bytes.TrimSpace(*raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return trimmed
}

func applyChatSSE(data []byte, w io.Writer, usage *wire.Usage, served *string, done *bool) (bool, error) {
	var chunk struct {
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Error *json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &chunk) != nil {
		return false, nil
	}
	if errRaw := nonNullJSON(chunk.Error); len(errRaw) > 0 {
		return false, fmt.Errorf("upstream error mid-stream: %s", errRaw)
	}
	if chunk.Model != "" {
		*served = chunk.Model
	}
	if chunk.Usage != nil {
		usage.InputTokens, usage.OutputTokens = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
	}
	for _, ch := range chunk.Choices {
		if ch.FinishReason != "" {
			*done = true
		}
		if ch.Delta.Content != "" {
			if _, err := io.WriteString(w, ch.Delta.Content); err != nil {
				return false, err
			}
		}
	}
	return false, nil
}

func applyResponsesSSE(data []byte, w io.Writer, usage *wire.Usage, served *string, done *bool) (bool, error) {
	var ev struct {
		Type     string `json:"type"`
		Delta    string `json:"delta"`
		Response *struct {
			Model string `json:"model"`
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return false, nil
	}
	switch ev.Type {
	case "response.output_text.delta":
		_, err := io.WriteString(w, ev.Delta)
		return false, err
	case "response.completed":
		*done = true
		if ev.Response != nil {
			*served = cmp.Or(ev.Response.Model, *served)
			if ev.Response.Usage != nil {
				usage.InputTokens, usage.OutputTokens = ev.Response.Usage.InputTokens, ev.Response.Usage.OutputTokens
			}
		}
		return true, nil
	case "response.failed", "response.incomplete", "error":
		return false, fmt.Errorf("responses stream %s: %s", ev.Type, string(data[:min(len(data), 300)]))
	}
	return false, nil
}

type anthropicSSEEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens         int `json:"input_tokens"`
			OutputTokens        int `json:"output_tokens"`
			CacheCreationTokens int `json:"cache_creation_input_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Delta *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func applyAnthropicSSE(data []byte, w io.Writer, usage *wire.Usage, served *string, done *bool) (bool, error) {
	var ev anthropicSSEEvent
	if json.Unmarshal(data, &ev) != nil {
		return false, nil
	}
	switch ev.Type {
	case "message_start":
		applyAnthropicStart(ev, usage, served)
	case "content_block_delta":
		return applyAnthropicDelta(ev, w)
	case "message_delta":
		if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
			usage.OutputTokens = ev.Usage.OutputTokens
		}
	case "message_stop":
		*done = true
		return true, nil
	case "error":
		return false, fmt.Errorf("anthropic stream error: %s", string(data[:min(len(data), 300)]))
	}
	return false, nil
}

func applyAnthropicStart(ev anthropicSSEEvent, usage *wire.Usage, served *string) {
	if ev.Message == nil {
		return
	}
	*served = ev.Message.Model
	u := ev.Message.Usage
	usage.InputTokens, usage.OutputTokens = u.InputTokens, u.OutputTokens
	usage.CacheCreationTokens, usage.CacheReadTokens = u.CacheCreationTokens, u.CacheReadTokens
}

func applyAnthropicDelta(ev anthropicSSEEvent, w io.Writer) (bool, error) {
	if ev.Delta == nil || ev.Delta.Type != "text_delta" {
		return false, nil
	}
	_, err := io.WriteString(w, ev.Delta.Text)
	return false, err
}
