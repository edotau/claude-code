package serving

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// CheckTarget is one endpoint to probe: ID is what the table shows, Wire the served endpoint name.
type CheckTarget struct {
	ID        string
	Wire      string
	Responses bool
	Missing   bool
}

// CheckResult is one probe outcome; Status is ok, fail, transient or missing, Tools yes, no or "" (unknown).
type CheckResult struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	HTTP      int       `json:"http"`
	LatencyMS int64     `json:"latency_ms"`
	Tools     string    `json:"tools"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitzero"`
}

// CheckOptions tunes Probe; zero values mean http.DefaultClient, 4 workers and a 30s request timeout.
type CheckOptions struct {
	Client       *http.Client
	Concurrency  int
	Timeout      time.Duration
	ClientRoutes bool // opt in to client-equivalent serving routes and larger bounded probe token budgets
}

const probePrompt = "Reply with OK."

var checkChatTool = map[string]any{"type": "function", "function": map[string]any{
	"name": "ping", "description": "Return pong.",
	"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
}}

var checkResponsesTool = map[string]any{"type": "function", "name": "ping", "description": "Return pong.",
	"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}

// Probe sends one tiny request per target (bounded concurrency) and returns results in target order.
func Probe(ctx context.Context, p *providers.Provider, targets []CheckTarget, o CheckOptions) ([]CheckResult, error) {
	host := providers.PresetHost(p.Host)
	if host == "" {
		return nil, fmt.Errorf("%s has no https workspace host to probe", p.Name)
	}
	chatBase, respBase := host+"/serving-endpoints", p.Routes[providers.ProtocolResponses]
	if respBase == "" {
		respBase = chatBase
	}
	o.Client = cmp.Or(o.Client, http.DefaultClient)
	if o.ClientRoutes {
		if _, err := NewScope(p, ""); err != nil {
			return nil, err
		}
		for _, t := range targets {
			if _, _, err := clientProbeRoute(p, t); !t.Missing && err != nil {
				return nil, err
			}
		}
		client := *o.Client
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		o.Client = &client
	}
	o.Concurrency = max(cmp.Or(o.Concurrency, 4), 1)
	o.Timeout = cmp.Or(o.Timeout, 30*time.Second)
	out := make([]CheckResult, len(targets))
	sem := make(chan struct{}, o.Concurrency)
	var wg sync.WaitGroup
	for i, t := range targets {
		if t.Missing {
			out[i] = CheckResult{ID: t.ID, Status: "missing"}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			out[i] = probeOne(ctx, p, o, t, chatBase, respBase)
		}()
	}
	wg.Wait()
	return out, ctx.Err()
}

// StatusTransient marks a probe lost to a passing condition (timeout, 429, 5xx, mint error), not a model verdict.
const StatusTransient = "transient"

// TransientHTTP reports a status that is a passing condition rather than a verdict on the model.
func TransientHTTP(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
}

func probeOne(ctx context.Context, p *providers.Provider, o CheckOptions, t CheckTarget, chatBase, respBase string) CheckResult {
	r := CheckResult{ID: t.ID}
	url, body := chatBase+"/"+t.Wire+"/invocations", chatBody
	if t.Responses {
		url, body = respBase+"/responses", func(tools bool) map[string]any { return responsesBody(t.Wire, tools) }
	}
	anthropic := false
	if o.ClientRoutes {
		route, dialect, err := clientProbeRoute(p, t)
		if err != nil {
			return CheckResult{ID: t.ID, Status: "fail", Error: "client route unavailable"}
		}
		url, anthropic = route, dialect == providers.ProtocolAnthropic
		body = func(tools bool) map[string]any { return clientProbeBody(t.Wire, dialect, tools) }
	}
	rep, err := probeBudget(ctx, p, o, ProbeCall{URL: url, Body: body(true), Anthropic: anthropic})
	r.HTTP, r.LatencyMS = rep.HTTP, rep.LatencyMS
	switch {
	case err != nil:
		r.Status, r.Error = StatusTransient, err.Error()
	case rep.HTTP/100 == 2:
		r.Status, r.Tools = "ok", "yes"
	default:
		r.Status, r.Error = "fail", fmt.Sprintf("HTTP %d: %s", rep.HTTP, TruncateMsg([]byte(rep.Body)))
		if TransientHTTP(rep.HTTP) {
			r.Status = StatusTransient
			return r
		}
		if rep.Denied { // a spend-budget 403 denies the endpoint, not the tools: no tools-less retry
			return r
		}
		rep2, err2 := probeBudget(ctx, p, o, ProbeCall{URL: url, Body: body(false), Anthropic: anthropic})
		switch {
		case err2 == nil && rep2.HTTP/100 == 2:
			r.Status, r.HTTP, r.LatencyMS, r.Tools, r.Error = "ok", rep2.HTTP, rep2.LatencyMS, "no", ""
		case err2 != nil:
			r.Status, r.Error = StatusTransient, "tools-less retry: "+err2.Error()
		case TransientHTTP(rep2.HTTP):
			r.Status, r.HTTP, r.LatencyMS = StatusTransient, rep2.HTTP, rep2.LatencyMS
			r.Error = fmt.Sprintf("tools-less retry: HTTP %d: %s", rep2.HTTP, TruncateMsg([]byte(rep2.Body)))
		}
	}
	return r
}

// clientProbeRoute follows OpenCode's menu partition: explicit Responses, native Claude, then chat.
func clientProbeRoute(p *providers.Provider, t CheckTarget) (string, string, error) {
	dialect, suffix := providers.ProtocolOpenAI, "/chat/completions"
	_, claudeRoute := p.Route(providers.ProtocolAnthropic)
	switch {
	case t.Responses:
		dialect, suffix = providers.ProtocolResponses, "/responses"
	case claudeRoute && models.FamilyKind(t.ID) == models.FamilyClaude:
		dialect, suffix = providers.ProtocolAnthropic, "/v1/messages"
	}
	base, ok := p.Route(dialect)
	if !ok {
		return "", "", errors.New("requested model has no resolved client route")
	}
	return base + suffix, dialect, nil
}

func clientProbeBody(model, dialect string, tools bool) map[string]any {
	if dialect == providers.ProtocolResponses {
		b := responsesBody(model, tools)
		b["max_output_tokens"] = 128
		return b
	}
	b := chatBody(false)
	b["model"], b["max_tokens"] = model, 256
	if dialect == providers.ProtocolAnthropic {
		b["max_tokens"] = 32
		if tools {
			b["tools"] = []any{map[string]any{"name": "ping", "description": "Return pong.",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{}}}}
			b["tool_choice"] = map[string]any{"type": "auto"}
		}
	} else if tools {
		b["tools"], b["tool_choice"] = []any{checkChatTool}, "auto"
	}
	return b
}

func chatBody(tools bool) map[string]any {
	b := map[string]any{"messages": []map[string]any{{"role": "user", "content": probePrompt}}, "max_tokens": 8}
	if tools {
		b["tools"], b["tool_choice"] = []any{checkChatTool}, "auto"
	}
	return b
}

func responsesBody(model string, tools bool) map[string]any {
	b := map[string]any{"model": model, "input": probePrompt, "max_output_tokens": 16}
	if tools {
		b["tools"] = []any{checkResponsesTool}
	}
	return b
}

// budgetExhausted is Databricks' 400 text when a model spends its token budget before the first output token.
const budgetExhausted = "max_tokens or model output limit was reached"

// budgetRetryTokens is the one retry's budget: room for a reasoning model's hidden tokens on a one-word reply.
const budgetRetryTokens = 1024

// ProbeCall is one probe POST; a set Token is sent as the bearer (after p.Headers) instead of minting one.
type ProbeCall struct {
	URL       string
	Body      map[string]any
	Token     string
	Anthropic bool
}

// ProbeReply is one probe's answer: Body is the reply with credentials scrubbed, Denied a 403 naming a spend budget.
type ProbeReply struct {
	HTTP      int
	LatencyMS int64
	Body      string
	Denied    bool
}

// ProbeOnce sends c (resent once with a larger budget when a reasoning model ran out); zero options as in Probe.
func ProbeOnce(ctx context.Context, p *providers.Provider, o CheckOptions, c ProbeCall) (ProbeReply, error) {
	o.Client = cmp.Or(o.Client, http.DefaultClient)
	o.Timeout = cmp.Or(o.Timeout, 30*time.Second)
	return probeBudget(ctx, p, o, c)
}

// probeBudget is probeRequest, resent once with budgetRetryTokens when the tiny budget ran out (a reasoning model).
func probeBudget(ctx context.Context, p *providers.Provider, o CheckOptions, c ProbeCall) (ProbeReply, error) {
	r, err := probeRequest(ctx, p, o, c)
	if err != nil || r.HTTP != http.StatusBadRequest || !strings.Contains(r.Body, budgetExhausted) {
		return r, err
	}
	c.Body = maps.Clone(c.Body)
	for _, k := range []string{"max_tokens", "max_output_tokens"} {
		if _, ok := c.Body[k]; ok {
			c.Body[k] = budgetRetryTokens
		}
	}
	return probeRequest(ctx, p, o, c)
}

// probeRequest POSTs c.Body and returns the reply with its latency, or a named transport error.
func probeRequest(ctx context.Context, p *providers.Provider, o CheckOptions, c ProbeCall) (ProbeReply, error) {
	raw, err := json.Marshal(c.Body)
	if err != nil {
		return ProbeReply{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(raw))
	if err != nil {
		return ProbeReply{}, err
	}
	if err := authorizeProbe(ctx, p, req.Header, c.Token); err != nil {
		return ProbeReply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Anthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	start := time.Now()
	resp, err := o.Client.Do(req)
	if err != nil {
		return ProbeReply{LatencyMS: time.Since(start).Milliseconds()}, probeFailure(ctx, err, o.Timeout)
	}
	defer resp.Body.Close()
	msg, err := readBody(ctx, resp, o.Timeout)
	body := scrubCredentials(string(msg), req.Header)
	return ProbeReply{HTTP: resp.StatusCode, LatencyMS: time.Since(start).Milliseconds(), Body: body,
		Denied: resp.StatusCode == http.StatusForbidden && strings.Contains(strings.ToLower(body), "budget")}, err
}

// authorizeProbe sets the bearer from a pre-minted token over p.Headers, else mints through providers.Authorize.
func authorizeProbe(ctx context.Context, p *providers.Provider, h http.Header, tok string) error {
	if tok == "" {
		if err := providers.Authorize(ctx, p, h); err != nil {
			return errors.New("credential: " + err.Error())
		}
		return nil
	}
	h.Set("Authorization", "Bearer "+tok)
	for k, v := range p.Headers {
		h.Set(k, v)
	}
	return nil
}

// scrubCredentials masks every credential h carries wherever the reply echoes it.
func scrubCredentials(s string, h http.Header) string {
	for _, k := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
		if v := strings.TrimPrefix(h.Get(k), "Bearer "); v != "" {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}

// probeFailure names a transport failure by kind; the error text itself could echo the URL.
func probeFailure(ctx context.Context, err error, timeout time.Duration) error {
	var ne net.Error
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return fmt.Errorf("timeout after %s", timeout)
	case errors.As(err, new(*net.DNSError)):
		return errors.New("network error: DNS")
	case errors.As(err, new(*tls.CertificateVerificationError)):
		return errors.New("network error: TLS")
	}
	return errors.New("network error")
}

// readBody reads up to 1MiB; only a 2xx body failure is an error, with text that cannot echo the address.
func readBody(ctx context.Context, resp *http.Response, timeout time.Duration) ([]byte, error) {
	msg, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err == nil || resp.StatusCode/100 != 2 { // a non-2xx status stands even when its body is cut short
		return msg, nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return msg, fmt.Errorf("timeout after %s", timeout)
	}
	return msg, errors.New("read response body failed")
}

// TruncateMsg collapses whitespace and cuts b to 160 runes for one-line diagnostics.
func TruncateMsg(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if r := []rune(s); len(r) > 160 {
		s = string(r[:160]) + "..."
	}
	return s
}
