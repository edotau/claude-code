package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/providers"
)

// envelopeMap round-trips the envelope NewEnvelope builds, so the test pins wire names, not Go types.
func envelopeMap(t *testing.T, res wire.Result, err error) map[string]any {
	t.Helper()
	b, merr := json.Marshal(wire.NewEnvelope(res, err, "d1"))
	if merr != nil {
		t.Fatal(merr)
	}
	var m map[string]any
	if uerr := json.Unmarshal(b, &m); uerr != nil {
		t.Fatal(uerr)
	}
	return m
}

// soleHop runs a one-dispatch chain and returns its envelope's only hop.
func soleHop(t *testing.T, r *fakeRunner, req RunOptions) map[string]any {
	t.Helper()
	res, err := RunWithFallback(context.Background(), Single(r), req, wire.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	hops, _ := envelopeMap(t, res, err)["hops"].([]any)
	if len(hops) != 1 {
		t.Fatalf("hops = %v, want one", hops)
	}
	h, _ := hops[0].(map[string]any)
	return h
}

// Guarantee 3: an auth failure on hop 1 moves to hop 2, and the envelope names every hop tried.
func assertTwoHops(t *testing.T, env map[string]any) {
	t.Helper()
	hops, _ := env["hops"].([]any)
	if len(hops) != 2 {
		t.Fatalf("hops = %v, want both dispatches", env["hops"])
	}
	h1, _ := hops[0].(map[string]any)
	h2, _ := hops[1].(map[string]any)
	if h1["agent"] != "claude" || h1["error_kind"] != "auth" {
		t.Errorf("hop 1 = %v, want agent claude / error_kind auth", h1)
	}
	if h2["agent"] != "gemini" || (h2["error_kind"] != nil && h2["error_kind"] != "") {
		t.Errorf("hop 2 = %v, want agent gemini with no error_kind", h2)
	}
}

func TestEnvelopeHopsNameEveryDispatchTried(t *testing.T) {
	authEnv(t, `{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"AGAUTH_UNSET"},"models":{"opus":"m"}}`)
	first := &fakeRunner{name: "claude", errs: []error{fmt.Errorf("agauth: %w", providers.ErrNeedsLogin)}, emptyAnswer: true}
	second := &fakeRunner{name: "gemini", errs: []error{nil}}
	res, err := RunWithFallback(context.Background(), twoDispatches(first, second), RunOptions{Prompt: "p"}, wire.Discard, nil)
	if err != nil || res.Answer != "a" || len(second.seen) != 1 {
		t.Fatalf("res=%+v err=%v: want the second hop to answer", res, err)
	}
	env := envelopeMap(t, res, err)
	assertTwoHops(t, env)
	if fr, _ := env["fallback_reason"].(string); fr == "" || env["fallback"] != "claude" {
		t.Errorf("fallback = %v reason = %q: hops must not replace the existing fallback fields", env["fallback"], fr)
	}
}

// A single successful dispatch, claude or any other agent answering itself, names one hop and no fallback.
func TestEnvelopeHopsSingleDispatch(t *testing.T) {
	for name, build := range map[string]func(Engine) Chain{
		"claude": Single,
		"gemini": func(r Engine) Chain { return chainOf([]Engine{r}) },
	} {
		t.Run(name, func(t *testing.T) {
			r := &fakeRunner{name: name, errs: []error{nil}}
			res, err := RunWithFallback(context.Background(), build(r), RunOptions{Prompt: "p"}, wire.Discard, nil)
			if err != nil || res.Fallback != "" || res.FallbackReason != "" {
				t.Fatalf("res=%+v err=%v, want no fallback", res, err)
			}
			env := envelopeMap(t, res, err)
			if hops, _ := env["hops"].([]any); len(hops) != 1 {
				t.Errorf("hops = %v, want one", env["hops"])
			}
			if env["fallback"] != nil {
				t.Errorf("fallback = %v, want null", env["fallback"])
			}
		})
	}
}

// A 429 followed by success is one hop, no error_kind, whose errors list the recovered rate_limit attempt.
func TestHopKeepsRecoveredAttemptErrors(t *testing.T) {
	fastDefaultBackoff(t)
	r := &fakeRunner{name: "claude", errs: []error{&retry.Error{Kind: retry.KindRateLimit, Agent: "claude", Err: errors.New("HTTP 429")}, nil}}
	h := soleHop(t, r, RunOptions{Prompt: "p"})
	errs, _ := h["errors"].([]any)
	if h["error_kind"] != nil || len(errs) != 1 || errs[0].(map[string]any)["kind"] != "rate_limit" {
		t.Errorf("hop = %v, want no error_kind and one rate_limit error", h)
	}
}

// A transient credential failure that a retry recovers from is listed in the answering hop's errors.
func TestHopKeepsRecoveredGuardFailure(t *testing.T) {
	fastDefaultBackoff(t)
	flag := filepath.Join(t.TempDir(), "seen")
	name := fmt.Sprintf("hmg%d", time.Now().UnixNano())
	hmNamed(t, name, fmt.Sprintf("if [ -f %[1]s ]; then echo %[2]s; else touch %[1]s; echo 'connection reset' >&2; exit 1; fi", flag, fakeJWT("adb-1.azuredatabricks.net")))
	r := &fakeRunner{name: "claude", caps: Caps{Provider: Native}, errs: []error{nil}}
	h := soleHop(t, r, RunOptions{Prompt: "p", Provider: name})
	if errs, _ := h["errors"].([]any); len(errs) != 1 {
		t.Errorf("hop = %v, want the recovered credential failure in errors", h)
	}
}
