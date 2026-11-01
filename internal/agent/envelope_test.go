package agent_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

// RanOn reads the answering (final) hop: a fallback run names the original agent, an own-login run says so.
func TestRanOnFromHops(t *testing.T) {
	fb := agent.Result{Agent: "copilot", Model: "m2", Fallback: "codex", Hops: []agent.Hop{
		{Agent: "codex", Account: "work", ErrorKind: "quota"},
		{Agent: "copilot", Account: "personal", Profile: "dev", Model: "m2"}}}
	if got, want := agent.RanOn(fb), "ran on copilot · personal·dev · m2 · fallback from codex"; got != want {
		t.Errorf("fallback = %q, want %q", got, want)
	}
	own := agent.Result{Agent: "claude", Model: "opus", Hops: []agent.Hop{{Agent: "claude", Model: "opus"}}}
	if got, want := agent.RanOn(own), "ran on claude · own login · opus · fallback none"; got != want {
		t.Errorf("own login = %q, want %q", got, want)
	}
	if got := agent.NewEnvelope(fb, nil, "d").RanOn; got != agent.RanOn(fb) {
		t.Errorf("envelope ran_on = %q", got)
	}
	if agent.RanOn(agent.Result{}) != "" {
		t.Error("empty result must have no ran-on line")
	}
}

// A dispatch that never started a dispatch claims no run: no ran-on line, a did-not-run line with the reason.
func TestRanOnClaimsOnlyARunThatHappened(t *testing.T) {
	res := agent.Result{Agent: "codex", Status: agent.StatusError, FallbackReason: "codex: deactivated (CODEX_ACTIVE=off)"}
	if got := agent.RanOn(res); got != "" {
		t.Errorf("RanOn = %q, want empty", got)
	}
	if got, want := agent.DidNotRun(res), "did not run codex — deactivated (CODEX_ACTIVE=off)"; got != want {
		t.Errorf("DidNotRun = %q, want %q", got, want)
	}
	if agent.NewEnvelope(res, nil, "d").RanOn != "" {
		t.Error("envelope ran_on must stay empty")
	}
	ran := agent.Result{Agent: "codex", Status: agent.StatusError, Hops: []agent.Hop{{Agent: "codex"}}}
	if agent.RanOn(ran) == "" || agent.DidNotRun(ran) != "" {
		t.Error("a dispatch that started and failed did run")
	}
}

// The envelope's new fields are omitted, not null or empty, when unset.
func TestEnvelopeNewFieldsOmitEmpty(t *testing.T) {
	keys := func(env agent.Envelope) map[string]json.RawMessage {
		b, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	empty := keys(agent.NewEnvelope(agent.Result{Agent: "claude", Status: agent.StatusOK, Usage: agent.Usage{InputTokens: 1}}, nil, "d"))
	for _, k := range []string{"emulated", "structured", "schema_unchecked"} {
		if _, ok := empty[k]; ok {
			t.Errorf("%s present on an empty result", k)
		}
	}
	if strings.Contains(string(empty["usage"]), "cost_usd") {
		t.Errorf("usage = %s, cost_usd must be absent when zero", empty["usage"])
	}
	full := keys(agent.NewEnvelope(agent.Result{Agent: "claude", Status: agent.StatusOK, Emulated: []string{"--output-schema"},
		Structured: json.RawMessage(`{"a":1}`), SchemaUnchecked: []string{"pattern"}, Usage: agent.Usage{CostUSD: 0.5}}, nil, "d"))
	for k, v := range map[string]string{"emulated": `["--output-schema"]`, "structured": `{"a":1}`, "schema_unchecked": `["pattern"]`} {
		if string(full[k]) != v {
			t.Errorf("%s = %s, want %s", k, full[k], v)
		}
	}
	if !strings.Contains(string(full["usage"]), `"cost_usd":0.5`) {
		t.Errorf("usage = %s, want cost_usd", full["usage"])
	}
}
