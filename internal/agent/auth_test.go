package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type markCall struct{ account, reason string }

// authEnv seeds a default provider row and records route.MarkAuth calls instead of touching quota.json.
func authEnv(t *testing.T, row string) *[]markCall {
	t.Helper()
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"agauth","providers":{"agauth":`+row+`}}`)
	var calls []markCall
	prev := markAuth
	markAuth = func(a, r string) error { calls = append(calls, markCall{a, r}); return nil }
	t.Cleanup(func() { markAuth = prev })
	return &calls
}

const u2mNoToken = `{"kind":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"oauth"},"models":{"opus":"m"}}`

func twoLegs(first, second *fakeRunner) Chain {
	return Chain{Want: first.Name(), Legs: []Leg{{Agent: first, Account: first.Name() + ":agauth"}, {Agent: second}}}
}

// A4+B2: a sealed-auth provider gets a Recover; its login-required failure marks the leg's account and moves on.
func TestSealedAuthLegRecoversOnceThenMovesOn(t *testing.T) {
	calls := authEnv(t, u2mNoToken)
	first := &fakeRunner{name: "codex", errs: []error{errors.New("HTTP 401 unauthorized"), errors.New("HTTP 401 unauthorized")}, emptyAnswer: true}
	second := &fakeRunner{name: "gemini", errs: []error{nil}}

	res, err := RunWithFallback(context.Background(), twoLegs(first, second), Request{Prompt: "p"}, io.Discard, nil)
	if err != nil || res.Answer != "a" {
		t.Fatalf("res=%+v err=%v, want the second leg's answer", res, err)
	}
	if len(first.seen) != 1 {
		t.Fatalf("first leg attempts = %d, want 1: a failed Recover must not retry", len(first.seen))
	}
	if len(second.seen) != 1 {
		t.Fatalf("second leg attempts = %d, want it to run in the same dispatch", len(second.seen))
	}
	if len(*calls) != 1 || (*calls)[0].account != "codex:agauth" || (*calls)[0].reason == "" {
		t.Fatalf("markAuth calls = %+v, want one mark on codex:agauth", *calls)
	}
	if res.Fallback != "codex" || !strings.Contains(res.FallbackReason, "codex") {
		t.Errorf("fallback not reported: %+v", res)
	}
}

// A transient Recover failure (the auth.command itself failing) never marks the account.
func TestTransientRecoverFailureDoesNotMark(t *testing.T) {
	calls := authEnv(t, `{"kind":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","command":"exit 1"},"models":{"opus":"m"}}`)
	first := &fakeRunner{name: "codex", errs: []error{errors.New("HTTP 401 unauthorized")}, emptyAnswer: true}
	second := &fakeRunner{name: "gemini", errs: []error{nil}}
	_, _ = RunWithFallback(context.Background(), twoLegs(first, second), Request{Prompt: "p"}, io.Discard, nil)
	if len(*calls) != 0 {
		t.Fatalf("a transient recover failure condemned an account: %+v", *calls)
	}
}

// A first-attempt error that itself carries a login-required verdict (no Recover ran) is condemned too.
func TestWrappedLoginRequiredOnFirstAttemptMarksAndMovesOn(t *testing.T) {
	calls := authEnv(t, `{"kind":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"AGAUTH_UNSET"},"models":{"opus":"m"}}`)
	first := &fakeRunner{name: "claude", errs: []error{fmt.Errorf("agauth: %w", providers.ErrNeedsLogin)}, emptyAnswer: true}
	second := &fakeRunner{name: "gemini", errs: []error{nil}}
	res, err := RunWithFallback(context.Background(), twoLegs(first, second), Request{Prompt: "p"}, io.Discard, nil)
	if err != nil || res.Answer != "a" || len(second.seen) != 1 {
		t.Fatalf("res=%+v err=%v: expected a move-on to the second leg", res, err)
	}
	if len(*calls) != 1 || (*calls)[0].account != "claude:agauth" {
		t.Fatalf("markAuth calls = %+v", *calls)
	}
}

func TestCondemnAuthRules(t *testing.T) {
	needs := fmt.Errorf("x: %w", providers.ErrNeedsLogin)
	for name, c := range map[string]struct {
		e       *Error
		recover error
		want    bool
	}{
		"auth + condemning recover":      {&Error{Kind: KindAuth, Err: errors.New("401")}, needs, true},
		"auth + transient recover":       {&Error{Kind: KindAuth, Err: errors.New("401")}, errors.New("exit status 1"), false},
		"auth + no recover run":          {&Error{Kind: KindAuth, Err: errors.New("401")}, nil, false},
		"fatal error carrying the login": {&Error{Kind: KindFatal, Err: needs}, nil, true},
		"fatal ordinary":                 {&Error{Kind: KindFatal, Err: errors.New("boom")}, needs, false},
		"transient ordinary":             {&Error{Kind: KindTransient, Err: errors.New("503")}, nil, false},
	} {
		condemnAuth(c.e, c.recover)
		if got := c.e.AuthReason != ""; got != c.want {
			t.Errorf("%s: condemned = %v, want %v", name, got, c.want)
		}
	}
}

func TestNoteAuthNeedsAccountAndReason(t *testing.T) {
	var calls []markCall
	prev := markAuth
	markAuth = func(a, r string) error { calls = append(calls, markCall{a, r}); return nil }
	t.Cleanup(func() { markAuth = prev })
	noteAuth(Leg{Account: ""}, &Error{AuthReason: "r"})
	noteAuth(Leg{Account: "codex:x"}, &Error{})
	noteAuth(Leg{Account: "codex:x"}, errors.New("plain"))
	if len(calls) != 0 {
		t.Fatalf("noteAuth marked without an account or reason: %+v", calls)
	}
	noteAuth(Leg{Account: "codex:x"}, &Error{AuthReason: "r"})
	if len(calls) != 1 || calls[0].account != "codex:x" {
		t.Fatalf("calls = %+v", calls)
	}
}
