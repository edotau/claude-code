package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

// Given counts around the k/M boundary, Then a count that would round to 1000.0k spells as millions.
func TestCompactCountRoundsUpIntoMillions(t *testing.T) {
	for n, want := range map[int]string{
		950: "950", 23_400: "23.4k", 999_949: "999.9k", 999_950: "1.00M", 999_999: "1.00M", 1_840_000: "1.84M",
	} {
		if got := compactCount(n); got != want {
			t.Errorf("compactCount(%d) = %q, want %q", n, got, want)
		}
	}
}

// RanOn reads the answering (final) hop: a fallback run names the original agent, an own-login run says so.
func TestRanOnFromHops(t *testing.T) {
	fb := Result{Agent: "copilot", Model: "m2", Fallback: "codex", Hops: []Hop{
		{Agent: "codex", Account: "work", ErrorKind: "quota"},
		{Agent: "copilot", Account: "personal", Profile: "dev", Model: "m2"}}}
	if got, want := RanOn(fb), "ran on copilot · personal·dev · m2 · fallback from codex"; got != want {
		t.Errorf("fallback = %q, want %q", got, want)
	}
	own := Result{Agent: "claude", Model: "opus", Hops: []Hop{{Agent: "claude", Model: "opus"}}}
	if got, want := RanOn(own), "ran on claude · own login · opus · fallback none"; got != want {
		t.Errorf("own login = %q, want %q", got, want)
	}
	if got := NewEnvelope(fb, nil, "d").RanOn; got != RanOn(fb) {
		t.Errorf("envelope ran_on = %q", got)
	}
	if RanOn(Result{}) != "" {
		t.Error("empty result must have no ran-on line")
	}
}

// The ran-on line carries the run's reported tokens (all four counts) and wall time; a missing one is left out.
func TestRanOnCost(t *testing.T) {
	base := Result{Agent: "copilot", Model: "m", Hops: []Hop{{Agent: "copilot", Model: "m"}}}
	cases := []struct {
		usage Usage
		ms    int64
		want  string
	}{
		{Usage{InputTokens: 3, CacheCreationTokens: 20619, OutputTokens: 5}, 333369, " · 20.6k tok · 5m33s"},
		{Usage{InputTokens: 812004, CacheReadTokens: 1402113, OutputTokens: 96550}, 0, " · 2.31M tok"},
		{Usage{}, 1500, " · 2s"},
		{Usage{OutputTokens: 950}, 0, " · 950 tok"},
	}
	for _, c := range cases {
		res := base
		res.Usage, res.DurationMs = c.usage, c.ms
		if got, want := RanOn(res), "ran on copilot · own login · m · fallback none"+c.want; got != want {
			t.Errorf("RanOn(%+v, %dms) = %q, want %q", c.usage, c.ms, got, want)
		}
	}
}

// A dispatch that never started a dispatch claims no run: no ran-on line, a did-not-run line with the reason.
func TestRanOnClaimsOnlyARunThatHappened(t *testing.T) {
	res := Result{Agent: "codex", Status: StatusError, FallbackReason: "codex: deactivated (CODEX_ACTIVE=off)"}
	if got := RanOn(res); got != "" {
		t.Errorf("RanOn = %q, want empty", got)
	}
	if got, want := DidNotRun(res), "did not run codex — deactivated (CODEX_ACTIVE=off)"; got != want {
		t.Errorf("DidNotRun = %q, want %q", got, want)
	}
	if NewEnvelope(res, nil, "d").RanOn != "" {
		t.Error("envelope ran_on must stay empty")
	}
	ran := Result{Agent: "codex", Status: StatusError, Hops: []Hop{{Agent: "codex"}}}
	if RanOn(ran) == "" || DidNotRun(ran) != "" {
		t.Error("a dispatch that started and failed did run")
	}
}

// The envelope's new fields are omitted, not null or empty, when unset.
func TestEnvelopeNewFieldsOmitEmpty(t *testing.T) {
	keys := func(env Envelope) map[string]json.RawMessage {
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
	empty := keys(NewEnvelope(Result{Agent: "claude", Status: StatusOK, Usage: Usage{InputTokens: 1}}, nil, "d"))
	for _, k := range []string{"emulated", "structured", "schema_unchecked"} {
		if _, ok := empty[k]; ok {
			t.Errorf("%s present on an empty result", k)
		}
	}
	if strings.Contains(string(empty["usage"]), "cost_usd") {
		t.Errorf("usage = %s, cost_usd must be absent when zero", empty["usage"])
	}
	full := keys(NewEnvelope(Result{Agent: "claude", Status: StatusOK, Emulated: []string{"--output-schema"},
		Structured: json.RawMessage(`{"a":1}`), SchemaUnchecked: []string{"pattern"}, Usage: Usage{CostUSD: 0.5}}, nil, "d"))
	for k, v := range map[string]string{"emulated": `["--output-schema"]`, "structured": `{"a":1}`, "schema_unchecked": `["pattern"]`} {
		if string(full[k]) != v {
			t.Errorf("%s = %s, want %s", k, full[k], v)
		}
	}
	if !strings.Contains(string(full["usage"]), `"cost_usd":0.5`) {
		t.Errorf("usage = %s, want cost_usd", full["usage"])
	}
}
