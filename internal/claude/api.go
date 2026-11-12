// the one-shot HTTP calls of claude: the streaming Messages call (anthropic-sdk-go) and the chat-completions call.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"

	"github.com/edotau/claude-code/internal/network/egress"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

// apiMaxTokens caps a one-shot answer; reasoning models spend part of it thinking.
const apiMaxTokens = 16_000

// claudeOneShot is claude's tool-less one-shot via the anthropic SDK or openai route; ok is false for cli auth.
func claudeOneShot(ctx context.Context, req agent.RunOptions, sink wire.Sink) (res wire.Result, ok bool, err error) {
	_, t, err := agent.ResolveRequestTarget(req)
	if err != nil {
		return wire.Result{}, true, err
	}
	p := t.Provider
	res = wire.Result{Provider: p.Name, Model: t.Model}
	if p.Auth.Method() == providers.AuthCLI {
		return res, false, nil
	}
	res.Dropped = oneShotDropped(req)
	effort, err := oneShotEffort(req, t)
	if err != nil {
		return res, true, retry.Fatal("claude", err)
	}
	prompt, err := claudeAttach(req.Prompt, req.Attach, false)
	if err != nil {
		return wire.Result{}, true, err
	}
	var answer strings.Builder
	w := io.MultiWriter(wire.SinkWriter(sink), &answer)
	var usage wire.Usage
	var model string
	if base, ok := providers.AnthropicRoute(p, t.Model); ok {
		usage, model, err = streamMessages(ctx, apiRequest{
			BaseURL: base, Model: t.Model, Prompt: prompt, Effort: effort, MaxTokens: apiMaxTokens,
			Authorize: func(ctx context.Context, h http.Header) error { return providers.Authorize(ctx, p, h) },
		}, w)
	} else {
		if err := providers.ClaudeCodeError(p, t.Model); err != nil { // Responses-only: the one-shot has no such wire
			return res, true, retry.Fatal("claude", err)
		}
		if _, servable, why := providers.WireFor(p, t.Model); !servable {
			return res, true, retry.Fatal("claude", errors.New(why))
		}
		base, _ := p.Route(providers.ProtocolOpenAI)
		usage, model, err = chatStream(ctx, base, p, t.Model, prompt, effort, w)
	}
	if model != "" {
		res.Model = model
	}
	res.Answer, res.Usage = answer.String(), usage
	logAPIRun(ctx, res, err)
	return res, true, err
}

// oneShotDropped names the flags that need a process: the HTTP one-shot has none to carry --env or write a debug file.
func oneShotDropped(req agent.RunOptions) []string {
	var out []string
	if len(req.Env) > 0 {
		out = append(out, "--env")
	}
	if req.DebugFile != "" {
		out = append(out, "--debug-file="+req.DebugFile)
	}
	return out
}

// oneShotEffort is the flag's effort, else the stored record's, spelled as the CLI path spells it.
func oneShotEffort(req agent.RunOptions, t providers.Target) (string, error) {
	st, err := agent.ResolveSettings(agentid.Claude, req, t, true)
	if err != nil {
		return "", err
	}
	return claudeEffort(st.Effort)
}

// logAPIRun writes the one-shot's answer, usage or error to the dispatch log (the stream has no event boundaries).
func logAPIRun(ctx context.Context, res wire.Result, err error) {
	lg := wire.EventLogFor(ctx)
	if res.Answer != "" {
		lg.Emit("assistant", res.Answer)
	}
	if err != nil {
		lg.Emit("error", err.Error())
		return
	}
	lg.Emit("result", fmt.Sprintf("tokens in=%d out=%d", res.Usage.InputTokens, res.Usage.OutputTokens))
}

// chatClient bounds the wait for response headers; the body stream stays governed by ctx.
var chatClient = &http.Client{Transport: func() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 2 * time.Minute
	t.Proxy = egress.Proxy // cloned at init, before cli.Run installs it on the default transport
	return t
}()}

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
func chatStream(ctx context.Context, base string, p *providers.Provider, model, prompt, effort string, w io.Writer) (wire.Usage, string, error) {
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
		return wire.Usage{}, "", err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if err := providers.Authorize(ctx, p, hreq.Header); err != nil {
		return wire.Usage{}, "", err
	}
	resp, err := chatClient.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return wire.Usage{}, "", ctx.Err()
		}
		return wire.Usage{}, "", &httpError{msg: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
		after, _ := textutil.ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		return wire.Usage{}, "", &httpError{status: resp.StatusCode, after: max(after, 0),
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
func applyChatLine(line []byte, w io.Writer, usage *wire.Usage, served *string) (done bool, _ error) {
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
		*usage = wire.Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
	}
	return false, writeChatChoices(w, chunk)
}

func writeChatChoices(w io.Writer, chunk chatChunk) error {
	for _, c := range chunk.Choices {
		if c.Delta.Content != "" {
			if _, err := io.WriteString(w, c.Delta.Content); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseChatStream(r io.Reader, w io.Writer) (wire.Usage, string, error) {
	var usage wire.Usage
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

// apiRequest is one single-turn ask. Authorize sets auth headers per request; the SDK's env credential chain is off.
type apiRequest struct {
	BaseURL   string
	Model     string
	Prompt    string
	Effort    string // low|medium|high|xhigh|max; "" = model default
	MaxTokens int64
	Authorize func(context.Context, http.Header) error
}

// streamMessages sends r and writes text deltas to w as they arrive; it returns the usage and the served model.
func streamMessages(ctx context.Context, r apiRequest, w io.Writer) (wire.Usage, string, error) {
	client := anthropic.NewClient(
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(r.BaseURL),
		option.WithHTTPClient(chatClient), // the header timeout and egress proxy the chat route has
		option.WithMaxRetries(0),          // the caller's dispatch loop owns retries
		option.WithMiddleware(anthropicAuthorization(r)),
	)
	params := anthropic.MessageNewParams{
		Model:     r.Model,
		MaxTokens: r.MaxTokens,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(r.Prompt))},
	}
	if r.Effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(r.Effort)}
	}
	var res wire.Usage
	var model string
	stream := client.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	stopped := false
	for stream.Next() {
		ev := stream.Current().AsAny()
		_, isStop := ev.(anthropic.MessageStopEvent)
		stopped = stopped || isStop
		if err := applyMessageEvent(ev, &res, &model, w); err != nil {
			return res, model, err
		}
	}
	if err := stream.Err(); err != nil {
		return res, model, boxSDKError(err)
	}
	if !stopped {
		return res, model, fmt.Errorf("messages stream ended without message_stop: %w", io.ErrUnexpectedEOF) // retries
	}
	return res, model, nil
}

// applyMessageEvent folds one stream event into usage and model, and writes a text delta to w.
func applyMessageEvent(ev any, res *wire.Usage, model *string, w io.Writer) error {
	switch ev := ev.(type) {
	case anthropic.MessageStartEvent:
		*model = ev.Message.Model
		u := ev.Message.Usage
		res.InputTokens, res.OutputTokens = int(u.InputTokens), int(u.OutputTokens)
		res.CacheCreationTokens, res.CacheReadTokens = int(u.CacheCreationInputTokens), int(u.CacheReadInputTokens)
	case anthropic.ContentBlockDeltaEvent:
		if t, ok := ev.Delta.AsAny().(anthropic.TextDelta); ok {
			_, err := io.WriteString(w, t.Text)
			return err
		}
	case anthropic.MessageDeltaEvent:
		foldDeltaUsage(res, ev.Usage)
	}
	return nil
}

// foldDeltaUsage applies a message_delta's cumulative usage; input and cache counts may be absent mid-stream.
func foldDeltaUsage(res *wire.Usage, u anthropic.MessageDeltaUsage) {
	if u.InputTokens > 0 {
		res.InputTokens = int(u.InputTokens)
	}
	if u.CacheCreationInputTokens > 0 {
		res.CacheCreationTokens = int(u.CacheCreationInputTokens)
	}
	if u.CacheReadInputTokens > 0 {
		res.CacheReadTokens = int(u.CacheReadInputTokens)
	}
	res.OutputTokens = int(u.OutputTokens)
}

func anthropicAuthorization(r apiRequest) option.Middleware {
	return func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		req.Header.Del("X-Api-Key")
		req.Header.Del("Authorization")
		if r.Authorize != nil {
			if err := r.Authorize(req.Context(), req.Header); err != nil {
				return nil, err
			}
		}
		return next(req)
	}
}

func boxSDKError(err error) error {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	out := &httpError{status: apiErr.StatusCode, msg: fmt.Sprintf("HTTP %d: %s", apiErr.StatusCode, apiErr.RawJSON())}
	if apiErr.Response != nil {
		// Keep the SDK caller's integer-only header policy.
		v := apiErr.Response.Header.Get("Retry-After")
		if v == strings.TrimSpace(v) && !strings.Contains(v, ":") {
			out.after, _ = textutil.ParseRetryAfter(v, time.Now())
		}
	}
	return out
}
