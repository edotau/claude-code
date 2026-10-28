package providers

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/paths"
)

// CheckTarget is one endpoint to probe: ID is what the table shows, Wire the served endpoint name.
type CheckTarget struct {
	ID        string
	Wire      string
	Responses bool
	Missing   bool
}

// CheckResult is one probe outcome; Status is ok, fail or missing, Tools yes, no or "" (unknown).
type CheckResult struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	HTTP      int    `json:"http"`
	LatencyMS int64  `json:"latency_ms"`
	Tools     string `json:"tools"`
	Error     string `json:"error,omitempty"`
}

// CheckOptions tunes ProbeEndpoints; zero values mean http.DefaultClient, 4 workers and a 30s request timeout.
type CheckOptions struct {
	Client      *http.Client
	Concurrency int
	Timeout     time.Duration
}

const checkPrompt = "Reply with OK."

var checkChatTool = map[string]any{"type": "function", "function": map[string]any{
	"name": "ping", "description": "Return pong.",
	"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
}}

var checkResponsesTool = map[string]any{"type": "function", "name": "ping", "description": "Return pong.",
	"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}

// ProbeEndpoints sends one tiny request per target (bounded concurrency) and returns results in target order.
func ProbeEndpoints(ctx context.Context, p *Provider, targets []CheckTarget, o CheckOptions) ([]CheckResult, error) {
	host := presetHost(p.Host)
	if host == "" {
		return nil, fmt.Errorf("%s has no https workspace host to probe", p.Name)
	}
	chatBase, respBase := host+"/serving-endpoints", p.Routes[DialectResponses]
	if respBase == "" {
		respBase = chatBase
	}
	o.Client = cmp.Or(o.Client, http.DefaultClient)
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

func probeOne(ctx context.Context, p *Provider, o CheckOptions, t CheckTarget, chatBase, respBase string) CheckResult {
	r := CheckResult{ID: t.ID}
	url, body := chatBase+"/"+t.Wire+"/invocations", chatBody
	if t.Responses {
		url, body = respBase+"/responses", func(tools bool) map[string]any { return responsesBody(t.Wire, tools) }
	}
	code, ms, msg, err := probeRequest(ctx, p, o, url, body(true))
	r.HTTP, r.LatencyMS = code, ms
	switch {
	case err != nil:
		r.Status, r.Error = "fail", err.Error()
	case code/100 == 2:
		r.Status, r.Tools = "ok", "yes"
	default:
		r.Status, r.Error = "fail", fmt.Sprintf("HTTP %d: %s", code, msg)
		if code2, ms2, _, err2 := probeRequest(ctx, p, o, url, body(false)); err2 == nil && code2/100 == 2 {
			r.Status, r.HTTP, r.LatencyMS, r.Tools, r.Error = "ok", code2, ms2, "no", ""
		}
	}
	return r
}

func chatBody(tools bool) map[string]any {
	b := map[string]any{"messages": []map[string]any{{"role": "user", "content": checkPrompt}}, "max_tokens": 8}
	if tools {
		b["tools"], b["tool_choice"] = []any{checkChatTool}, "auto"
	}
	return b
}

func responsesBody(model string, tools bool) map[string]any {
	b := map[string]any{"model": model, "input": checkPrompt, "max_output_tokens": 16}
	if tools {
		b["tools"] = []any{checkResponsesTool}
	}
	return b
}

// probeRequest POSTs body and returns status, latency, a truncated server message and any transport error.
func probeRequest(ctx context.Context, p *Provider, o CheckOptions, url string, body map[string]any) (int, int64, string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, 0, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := Authorize(ctx, p, req.Header); err != nil {
		return 0, 0, "", errors.New("credential: " + err.Error())
	}
	start := time.Now()
	resp, err := o.Client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return 0, time.Since(start).Milliseconds(), "", fmt.Errorf("timeout after %s", o.Timeout)
		}
		return 0, time.Since(start).Milliseconds(), "", errors.New("request failed") // url.Error text could echo the URL
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, time.Since(start).Milliseconds(), truncateMsg(msg), nil
}

func truncateMsg(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if r := []rune(s); len(r) > 160 {
		s = string(r[:160]) + "..."
	}
	return s
}

// CheckFamily is the id's leading letters, e.g. claude, gpt, gemini, glm, qwen.
func CheckFamily(id string) string {
	id = stripSpellingPrefix(id)
	if i := strings.IndexFunc(id, func(r rune) bool { return !unicode.IsLetter(r) }); i > 0 {
		return id[:i]
	}
	return id
}

// SortCheckResults orders by family, then latency (non-ok last in family), then id.
func SortCheckResults(rs []CheckResult) {
	slices.SortStableFunc(rs, func(a, b CheckResult) int {
		if c := cmp.Compare(CheckFamily(a.ID), CheckFamily(b.ID)); c != 0 {
			return c
		}
		la, lb := a.LatencyMS, b.LatencyMS
		if a.Status != "ok" {
			la = 1 << 60
		}
		if b.Status != "ok" {
			lb = 1 << 60
		}
		return cmp.Or(cmp.Compare(la, lb), cmp.Compare(a.ID, b.ID))
	})
}

// CheckCache is the persisted result set, scoped to one provider.
type CheckCache struct {
	CheckedAt time.Time     `json:"checked_at"`
	Provider  string        `json:"provider"`
	Results   []CheckResult `json:"results"`
}

// CheckCachePath is state/harness/opencode-models-check.json.
func CheckCachePath() string { return filepath.Join(paths.StateDir(), "opencode-models-check.json") }

// SaveCheckCache writes c (no secrets) with mode 0600.
func SaveCheckCache(c CheckCache) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := CheckCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// LoadCheckCache returns the cached results by id when the cache exists and belongs to provider.
func LoadCheckCache(provider string) (map[string]CheckResult, bool) {
	b, err := os.ReadFile(CheckCachePath())
	if err != nil {
		return nil, false
	}
	var c CheckCache
	if json.Unmarshal(b, &c) != nil || c.Provider != provider {
		return nil, false
	}
	m := make(map[string]CheckResult, len(c.Results))
	for _, r := range c.Results {
		m[r.ID] = r
	}
	return m, true
}
