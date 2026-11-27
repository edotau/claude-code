// Package jev asks the local Open-Jev classifier which model tier and agent a brief needs; callers fail open on error.
package jev

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/platform/proc"
)

const (
	defaultURL = "http://127.0.0.1:11401"
	urlEnv     = "HARNESS_JEV_URL"
	maxBrief   = 3500
	timeout    = 3 * time.Second // one question: ~0.5 s idle, 1.7-1.9 s measured while MPS trains; hooks fail open
	maxBody    = 1 << 16
)

// Error classes recorded on a dispatch; never a URL or body.
const (
	ClassDown        = "down"
	ClassTimeout     = "timeout"
	ClassBadResponse = "bad-response"
	ClassOff         = "off"
)

// Typed errors, one per class.
var (
	ErrOff         = errors.New("jev: off")
	ErrDown        = errors.New("jev: server down")
	ErrTimeout     = errors.New("jev: timeout")
	ErrBadResponse = errors.New("jev: bad response")
)

// Result is one question's pick, its top probability and the call's duration.
type Result struct {
	Pick       string
	Confidence float64
	Ms         int64
}

// Class is the short error class of err, "" for nil.
func Class(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrOff):
		return ClassOff
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout
	case errors.Is(err, ErrBadResponse):
		return ClassBadResponse
	}
	return ClassDown
}

type answer struct {
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Agents is the fixed list the agent question picks from.
var Agents = []string{"claude", "codex", "copilot", "gemini", "opencode", "pi", "local"}

const agentQuestion = `{"type":"choice","instructions":"Pick the agent type best suited to this coding-agent task by vendor strength.","criteria":{` +
	`"claude":"Careful multi-file engineering, review, planning and judgment.",` +
	`"codex":"Focused code edits and refactors with clear acceptance checks.",` +
	`"copilot":"Small routine edits, tests and docs in a known repo.",` +
	`"gemini":"Wide codebase sweeps, long-context reading and security-sensitive review.",` +
	`"opencode":"Routine scoped edits that a cheaper model can do.",` +
	`"pi":"Predetermined mechanical edits, renames, gate runs and lane work.",` +
	`"local":"Short, low-risk tasks that need no hosted model."}}`

const needsQuestion = `{"type":"choice","instructions":"Does task B need the result of task A before it can start?","criteria":{` +
	`"yes":"B reads, builds on or edits what A produces, or must run after A finishes.",` +
	`"no":"B can start now: it touches different files and needs nothing A produces."}}`

// Tier classifies the 3-way tier of the first 3500 characters of brief.
func Tier(ctx context.Context, brief string) (Result, error) {
	return one(ctx, clip(brief), "tier", assets.JevTier(), "fast", "standard", "deep")
}

// Agent classifies only the agent type of brief (7 candidates); Pick is one of Agents.
func Agent(ctx context.Context, brief string) (Result, error) {
	return one(ctx, clip(brief), "agent", json.RawMessage(agentQuestion), Agents...)
}

// Needs asks whether task b needs task a's result first; Pick is "yes" or "no".
func Needs(ctx context.Context, a, b string) (Result, error) {
	state := "Task A:\n" + clip(a) + "\n\nTask B:\n" + clip(b)
	return one(ctx, state, "needs", json.RawMessage(needsQuestion), "yes", "no")
}

// one asks the single question name about state and returns its pick and top probability.
func one(ctx context.Context, state, name string, q json.RawMessage, choices ...string) (Result, error) {
	res, body, err := ask(ctx, state, map[string]json.RawMessage{name: q})
	if err != nil {
		return res, err
	}
	a, err := parse(body, name, choices...)
	if err != nil {
		return res, err
	}
	res.Pick, res.Confidence = a.Choice, topProb(a)
	return res, nil
}

func ask(ctx context.Context, state string, qs map[string]json.RawMessage) (Result, []byte, error) {
	start := time.Now()
	body, err := post(ctx, timeout, strings.TrimRight(proc.Value(ctx, urlEnv), "/"), defaultURL, "/v1/systemone",
		map[string]any{"state": state, "questions": qs})
	if errors.Is(err, ErrOff) {
		return Result{}, nil, err
	}
	return Result{Ms: time.Since(start).Milliseconds()}, body, err
}

func clip(s string) string {
	if r := []rune(s); len(r) > maxBrief {
		return string(r[:maxBrief])
	}
	return s
}

func topProb(a answer) float64 {
	top := 0.0
	for _, p := range a.Probabilities {
		top = max(top, p)
	}
	return top
}

// post sends payload as JSON to base (def when "") plus path within d; "off", or "" under test, is ErrOff.
func post(ctx context.Context, d time.Duration, base, def, path string, payload any) ([]byte, error) {
	if base == "off" || base == "" && testing.Testing() { // a test binary never reaches the real server
		return nil, ErrOff
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cmp.Or(base, def)+path, bytes.NewReader(b))
	if err != nil {
		return nil, ErrDown
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ErrTimeout
		}
		return nil, ErrDown
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ErrTimeout
		}
		return nil, ErrBadResponse
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrBadResponse, resp.StatusCode)
	}
	return body, nil
}

func parse(body []byte, name string, choices ...string) (answer, error) {
	var out struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return answer{}, ErrBadResponse
	}
	var a answer
	if err := json.Unmarshal(out.Answers[name], &a); err != nil {
		return answer{}, ErrBadResponse
	}
	if !slices.Contains(choices, a.Choice) || len(a.Probabilities) == 0 {
		return answer{}, ErrBadResponse
	}
	sum := 0.0
	for _, p := range a.Probabilities {
		if p < 0 || p > 1 {
			return answer{}, ErrBadResponse
		}
		sum += p
	}
	if sum > 1.02 || topProb(a)-a.Probabilities[a.Choice] > 1e-6 {
		return answer{}, ErrBadResponse
	}
	return a, nil
}
