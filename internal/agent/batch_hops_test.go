package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// envelopeMap round-trips the envelope NewEnvelope builds, so the test pins wire names, not Go types.
func envelopeMap(t *testing.T, res Result, err error) map[string]any {
	t.Helper()
	b, merr := json.Marshal(NewEnvelope(res, err, "d1"))
	if merr != nil {
		t.Fatal(merr)
	}
	var m map[string]any
	if uerr := json.Unmarshal(b, &m); uerr != nil {
		t.Fatal(uerr)
	}
	return m
}

// Guarantee 3: an auth failure on hop 1 moves to hop 2, and the envelope names every hop tried.
func TestEnvelopeHopsNameEveryLegTried(t *testing.T) {
	authEnv(t, `{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"AGAUTH_UNSET"},"models":{"opus":"m"}}`)
	first := &fakeRunner{name: "claude", errs: []error{fmt.Errorf("agauth: %w", providers.ErrNeedsLogin)}, emptyAnswer: true}
	second := &fakeRunner{name: "gemini", errs: []error{nil}}
	res, err := RunWithFallback(context.Background(), twoLegs(first, second), Request{Prompt: "p"}, Discard, nil)
	if err != nil || res.Answer != "a" || len(second.seen) != 1 {
		t.Fatalf("res=%+v err=%v: want the second hop to answer", res, err)
	}
	env := envelopeMap(t, res, err)
	hops, _ := env["hops"].([]any)
	if len(hops) != 2 {
		t.Fatalf("hops = %v, want both legs", env["hops"])
	}
	h1, _ := hops[0].(map[string]any)
	h2, _ := hops[1].(map[string]any)
	if h1["agent"] != "claude" || h1["error_kind"] != "auth" {
		t.Errorf("hop 1 = %v, want agent claude / error_kind auth", h1)
	}
	if h2["agent"] != "gemini" || (h2["error_kind"] != nil && h2["error_kind"] != "") {
		t.Errorf("hop 2 = %v, want agent gemini with no error_kind", h2)
	}
	if fr, _ := env["fallback_reason"].(string); fr == "" || env["fallback"] != "claude" {
		t.Errorf("fallback = %v reason = %q: hops must not replace the existing fallback fields", env["fallback"], fr)
	}
}

// A single successful leg keeps the envelope free of fallback and names exactly one hop.
func TestEnvelopeHopsSingleLeg(t *testing.T) {
	r := &fakeRunner{name: "claude", errs: []error{nil}}
	res, err := RunWithFallback(context.Background(), Single(r), Request{Prompt: "p"}, Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := envelopeMap(t, res, err)
	if hops, _ := env["hops"].([]any); len(hops) != 1 {
		t.Errorf("hops = %v, want one", env["hops"])
	}
	if env["fallback"] != nil {
		t.Errorf("fallback = %v, want null", env["fallback"])
	}
}

// A cancelled context stops the leg loop before any leg runs.
func TestRunWithFallbackCancelledBeforeLegNeverRuns(t *testing.T) {
	r := &fakeRunner{name: "claude", errs: []error{nil}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := RunWithFallback(ctx, Single(r), Request{Prompt: "p"}, Discard, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if len(r.seen) != 0 {
		t.Errorf("runner ran %d times under a cancelled context", len(r.seen))
	}
}
