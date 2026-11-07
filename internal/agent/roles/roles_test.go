package roles

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	_ "github.com/edotau/claude-code/internal/gemini" // registers the gemini schema (ReadNeedsTools)
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// fakeStream swaps streamLeg for a stub keyed by model; models in failOn return a failed envelope.
func fakeStream(t *testing.T, failOn ...string) *[]agent.RunOptions {
	t.Helper()
	var mu sync.Mutex
	var seen []agent.RunOptions
	orig := streamLeg
	streamLeg = func(_ *agent.Client, _ context.Context, o agent.RunOptions) <-chan agent.Event {
		mu.Lock()
		seen = append(seen, o)
		mu.Unlock()
		env := agent.Envelope{Status: "ok", Agent: o.Agent, Model: o.Model, Answer: "answer " + o.Model}
		for _, m := range failOn {
			if m == o.Model {
				env = agent.FailureEnvelope(o.Agent, errors.New("boom"))
			}
		}
		ch := make(chan agent.Event, 1)
		ch <- agent.ResultEvent(env)
		close(ch)
		return ch
	}
	t.Cleanup(func() { streamLeg = orig })
	return &seen
}

func mustRole(t *testing.T, name string) Role {
	t.Helper()
	r, err := Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRoleExploreLegsOnePerPart(t *testing.T) {
	vaulttest.Use(t)
	legs := Legs(mustRole(t, "explore"), Request{Task: "t", Parts: []string{"a", "b"}, WorkDir: "/w", Web: true})
	if len(legs) != 2 || legs[0].Label != "a" || legs[1].Label != "b" {
		t.Fatalf("want legs a,b; got %+v", legs)
	}
	o := legs[1].Opts
	if o.Agent != "gemini" || !o.Tools || o.WorkDir != "/w" || o.Prompt != Prompt("b", true) {
		t.Errorf("explore opts wrong: %+v", o)
	}
}

func TestRoleLegsCarryOutputSchema(t *testing.T) {
	vaulttest.Use(t)
	const schema = `{"type":"object"}`
	for _, tc := range []struct{ role, models string }{{"explore", ""}, {"fleet", ""}, {"compare", ""}} {
		legs := Legs(mustRole(t, tc.role), Request{Task: "t", Parts: []string{"a"}, WorkDir: "/w", OutputSchema: schema})
		if len(legs) == 0 {
			t.Fatalf("%s: no legs", tc.role)
		}
		for _, l := range legs {
			if l.Err == "" && l.Opts.OutputSchema != schema {
				t.Errorf("%s leg %q: OutputSchema = %q, want %q", tc.role, l.Label, l.Opts.OutputSchema, schema)
			}
		}
	}
}

func TestRoleExploreNoPartsUsesTask(t *testing.T) {
	vaulttest.Use(t)
	legs := Legs(mustRole(t, "explore"), Request{Task: "whole", Web: false})
	if len(legs) != 1 || legs[0].Opts.Prompt != Prompt("whole", false) {
		t.Fatalf("want one leg on the task; got %+v", legs)
	}
}

func TestRoleFleetOneLeg(t *testing.T) {
	vaulttest.Use(t)
	legs := Legs(mustRole(t, "fleet"), Request{Task: "do it"})
	if len(legs) != 1 || legs[0].Label != "fleet" {
		t.Fatalf("want one fleet leg; got %+v", legs)
	}
	if o := legs[0].Opts; o.Agent != "copilot" || !o.Fleet || !o.Tools || o.Prompt != "do it" {
		t.Errorf("fleet opts wrong: %+v", o)
	}
}

func TestRoleCompareUnknownModelPreFailed(t *testing.T) {
	vaulttest.Use(t)
	seen := fakeStream(t)
	legs := Legs(mustRole(t, "compare"), Request{Task: "q", Models: []string{"claude-haiku-5-5", "bogus"}})
	if legs[0].Err != "" || legs[1].Err != "not on the opencode menu" {
		t.Fatalf("errs: %q %q", legs[0].Err, legs[1].Err)
	}
	rep := Run(context.Background(), nil, mustRole(t, "compare"), legs, 4, nil)
	var out bytes.Buffer
	rep.Text(&out)
	if rep.Status != "partial" || len(*seen) != 1 || (*seen)[0].Model != "claude-haiku-5-5" {
		t.Errorf("status=%s streamed=%v", rep.Status, *seen)
	}
	if !strings.HasSuffix(out.String(), "failed: bogus\n") || !strings.Contains(out.String(), "not on the opencode menu") {
		t.Errorf("text:\n%s", out.String())
	}
}

func TestRoleCompareDefaultsToRoleModels(t *testing.T) {
	vaulttest.Use(t)
	r := Role{Name: "compare", Agent: "opencode", Models: []string{"kimi-k3"}}
	legs := Legs(r, Request{Task: "q"})
	if len(legs) != 1 || legs[0].Opts.Model != "kimi-k3" || legs[0].Opts.Prompt != "q" {
		t.Fatalf("got %+v", legs)
	}
}

func TestRunStatusOkPartialError(t *testing.T) {
	vaulttest.Use(t)
	r := mustRole(t, "compare")
	mk := func(ms ...string) []Leg {
		var legs []Leg
		for _, m := range ms {
			legs = append(legs, Leg{Label: m, Opts: agent.RunOptions{Agent: "opencode", Model: m}})
		}
		return legs
	}
	for _, tc := range []struct {
		fail []string
		want string
	}{{nil, "ok"}, {[]string{"b"}, "partial"}, {[]string{"a", "b"}, "error"}} {
		fakeStream(t, tc.fail...)
		rep := Run(context.Background(), nil, r, mk("a", "b"), 2, nil)
		if rep.Status != tc.want {
			t.Errorf("fail=%v: status %s, want %s", tc.fail, rep.Status, tc.want)
		}
		if tc.want == "partial" && (rep.Legs[0].Envelope.Answer != "answer a" || rep.Legs[1].Envelope.Status == "ok") {
			t.Errorf("partial lost the surviving answer: %+v", rep.Legs)
		}
	}
}

func TestRoleVaultOverlayMovesExplore(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, rolesKey, `[{"name":"explore","agent":"opencode"}]`)
	r := mustRole(t, "explore")
	if r.Agent != "opencode" {
		t.Fatalf("overlay ignored: %+v", r)
	}
	if _, err := Lookup("fleet"); err == nil {
		t.Error("overlay replaces the table: fleet must be unknown")
	}
	if got := Legs(r, Request{Task: "x"})[0].Opts.Agent; got != "opencode" {
		t.Errorf("leg agent %q", got)
	}
}

func TestRunProgressTagsWholeLines(t *testing.T) {
	orig := streamLeg
	streamLeg = func(_ *agent.Client, _ context.Context, o agent.RunOptions) <-chan agent.Event {
		ch := make(chan agent.Event, 4)
		for _, s := range []string{"hel", "lo\nwor", "ld"} {
			ch <- agent.Event{Type: agent.EventText, Text: s}
		}
		ch <- agent.ResultEvent(agent.Envelope{Status: "ok"})
		close(ch)
		return ch
	}
	t.Cleanup(func() { streamLeg = orig })
	var out bytes.Buffer
	Run(context.Background(), nil, Role{Name: "fleet"}, []Leg{{Label: "x"}}, 1, &out)
	if got := out.String(); got != "[x] hello\n[x] world\n" {
		t.Errorf("progress = %q", got)
	}
}
