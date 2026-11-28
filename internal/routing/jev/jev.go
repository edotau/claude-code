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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
)

const (
	defaultURL   = "http://127.0.0.1:11401"
	urlEnv       = "HARNESS_JEV_URL"
	maxBrief     = 3500
	timeout      = 3 * time.Second // offline/CLI budget; hook callers supply a tighter context
	probeTimeout = 50 * time.Millisecond
	maxBody      = 1 << 16
)

// probeAddr is the host:port Probe dials for target; "" when target has no host.
func probeAddr(target string) string {
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// Probe checks whether Open-Jev's TCP port is listening within 50 ms, seeding backoff immediately when down.
func Probe(ctx context.Context) error {
	base := strings.TrimRight(proc.Value(ctx, urlEnv), "/")
	if base == "off" || base == "" && testing.Testing() {
		return ErrOff
	}
	target := cmp.Or(base, defaultURL)
	addr := probeAddr(target)
	if backedOff(target) || addr == "" {
		return ErrDown
	}
	d := net.Dialer{Timeout: probeTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		if ctx.Err() == nil {
			noteOutcome(target, ErrDown, 0)
		}
		return ErrDown
	}
	_ = conn.Close()
	return nil
}

// Server is the Jev server's state (up, down or off), its URL and the model it reports ("" when it reports none).
func Server(ctx context.Context) (state, target, model string) {
	target = cmp.Or(strings.TrimRight(proc.Value(ctx, urlEnv), "/"), defaultURL)
	switch err := Probe(ctx); {
	case errors.Is(err, ErrOff):
		return "off", target, ""
	case err != nil:
		return "down", target, ""
	}
	model, _ = health(ctx, target)
	return "up", target, model
}

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
	errServer      = errors.New("jev: server error")
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

// Classify asks tier and agent together in one request.
func Classify(ctx context.Context, brief string) (tier Result, agent Result, err error) {
	res, body, err := ask(ctx, clip(brief), map[string]json.RawMessage{
		"tier": assets.JevTier(), "agent": json.RawMessage(agentQuestion),
	})
	if err != nil {
		return res, res, err
	}
	t, terr := parse(body, "tier", "fast", "standard", "deep")
	a, aerr := parse(body, "agent", Agents...)
	tier = Result{Pick: t.Choice, Confidence: topProb(t), Ms: res.Ms}
	agent = Result{Pick: a.Choice, Confidence: topProb(a), Ms: res.Ms}
	return tier, agent, errors.Join(terr, aerr)
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
	base := strings.TrimRight(proc.Value(ctx, urlEnv), "/")
	if base == "off" || base == "" && testing.Testing() {
		return Result{}, nil, ErrOff
	}
	target := cmp.Or(base, defaultURL)
	if backedOff(target) {
		return Result{}, nil, ErrDown
	}
	body, err := post(ctx, timeout, base, defaultURL, "/v1/systemone",
		map[string]any{"state": state, "questions": qs})
	if errors.Is(err, ErrOff) {
		return Result{}, nil, err
	}
	if ctx.Err() == nil { // caller-local cancel/deadline never poisons shared backoff
		noteOutcome(target, err, time.Since(start))
	}
	return Result{Ms: time.Since(start).Milliseconds()}, body, err
}

// Backoff marker: a short cross-process TTL so a down or slow Jev costs one timeout, not one per process.
const backoffTTL = 30 * time.Second

// slowThreshold: a real brief takes about 1 s, and 800 ms backed the server off on most replies; tests shrink it.
var slowThreshold = 2 * time.Second

type backoffState struct {
	Until int64 `json:"until,omitempty"`
	Slow  int   `json:"slow,omitempty"`
}

var backoffMemory = struct {
	sync.Mutex
	path   string
	states map[string]backoffState
	// pendingLocal holds updates that could not take the file lock; they win over older disk state until expiry.
	pendingLocal map[string]backoffState
}{states: make(map[string]backoffState), pendingLocal: make(map[string]backoffState)}

var (
	backoffNow  = time.Now
	backoffPath = func() string {
		if testing.Testing() {
			return ""
		}
		return filepath.Join(paths.StateDir(), "jev-backoff")
	}
)

// readBackoffs accepts both the shared target map and the original single-target marker.
func readBackoffs(p string) map[string]backoffState {
	states := make(map[string]backoffState)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	if json.Unmarshal(b, &states) == nil && states != nil {
		return states
	}
	states = make(map[string]backoffState)
	rawExp, url, ok := strings.Cut(string(b), "\t")
	if exp, err := strconv.ParseInt(rawExp, 10, 64); ok && err == nil {
		states[url] = backoffState{Until: exp}
	}
	return states
}

// backedOff checks the shared state when available, otherwise the process-local state.
func backedOff(target string) bool {
	backoffMemory.Lock()
	defer backoffMemory.Unlock()
	return backoffNow().UnixNano() < backoffStates(backoffPath())[target].Until
}

// backoffStates prefers shared state and falls back to memory if the marker is unavailable; the mutex is held.
func backoffStates(p string) map[string]backoffState {
	if backoffMemory.path != p {
		backoffMemory.path = p
		backoffMemory.states = make(map[string]backoffState)
		backoffMemory.pendingLocal = make(map[string]backoffState)
	}
	states := backoffMemory.states
	if p != "" {
		if disk := readBackoffs(p); disk != nil {
			states = disk
		}
	}
	return mergePending(states)
}

// mergePending overlays unexpired unsaved local backoff on states and drops expired entries; the mutex is held.
func mergePending(states map[string]backoffState) map[string]backoffState {
	now := backoffNow().UnixNano()
	for t, s := range backoffMemory.pendingLocal {
		switch {
		case s.Until != 0 && now >= s.Until:
			delete(backoffMemory.pendingLocal, t)
		case s.Until > states[t].Until || s.Until == states[t].Until && s.Slow > states[t].Slow:
			states[t] = s
		}
	}
	return states
}

// noteOutcome records live calls only; caller-local cancellation must not poison shared health.
func noteOutcome(target string, err error, elapsed time.Duration) {
	backoffMemory.Lock()
	defer backoffMemory.Unlock()
	p := backoffPath()
	update := func() error {
		states := backoffStates(p)
		next := nextBackoff(states[target], err, elapsed)
		delete(states, target)
		if next != (backoffState{}) {
			states[target] = next
		}
		backoffMemory.states = states
		if p != "" {
			if len(states) == 0 {
				_ = os.Remove(p)
			} else if b, e := json.Marshal(states); e == nil {
				writeBackoffAtomic(p, b)
			}
			clear(backoffMemory.pendingLocal)
		}
		return nil
	}
	if p == "" {
		_ = update()
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if ran, _ := paths.TryFileLock(p+".lock", 10*time.Millisecond, update); !ran {
		states := backoffStates(p)
		next := nextBackoff(states[target], err, elapsed)
		states[target] = next
		backoffMemory.pendingLocal[target] = next
		backoffMemory.states = states
	}
}

func nextBackoff(s backoffState, err error, elapsed time.Duration) backoffState {
	if s.Until != 0 && backoffNow().UnixNano() >= s.Until {
		s = backoffState{}
	}
	switch {
	case errors.Is(err, ErrDown), errors.Is(err, ErrTimeout), errors.Is(err, errServer):
		s = backoffState{Until: backoffNow().Add(backoffTTL).UnixNano()}
	case elapsed > slowThreshold:
		s.Slow++
		if s.Slow >= 2 {
			s = backoffState{Until: backoffNow().Add(backoffTTL).UnixNano()}
		}
	case err == nil:
		s = backoffState{}
	default:
		s.Slow = 0
	}
	return s
}

func writeBackoffAtomic(p string, data []byte) {
	dir := filepath.Dir(p)
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".jev-backoff-*")
	if err != nil {
		return
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return
	}
	if os.Rename(tmp, p) != nil {
		_ = os.Remove(tmp)
	}
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
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: %w: status %d", ErrBadResponse, errServer, resp.StatusCode)
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
	if !slices.Contains(choices, a.Choice) || !normalized(a) {
		return answer{}, ErrBadResponse
	}
	return a, nil
}

// normalized reports whether a's probabilities are each in [0,1], sum to 1±0.02 and peak at its Choice.
func normalized(a answer) bool {
	sum := 0.0
	for _, p := range a.Probabilities {
		if p < 0 || p > 1 {
			return false
		}
		sum += p
	}
	return sum >= 0.98 && sum <= 1.02 && topProb(a)-a.Probabilities[a.Choice] <= 1e-6
}
