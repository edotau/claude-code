package agentid

import (
	"slices"
	"testing"

	_ "github.com/edotau/claude-code/internal/platform/nonet" // tests refuse real DNS: mock API calls
)

func TestAll(t *testing.T) {
	all := All()
	if want := []string{"claude", "codex", "copilot", "gemini", "opencode", "pi"}; !slices.Equal(all, want) {
		t.Fatalf("All() = %v, want %v", all, want)
	}
	for i, name := range all {
		if !Known(name) {
			t.Errorf("Known(%q) = false for a roster agent", name)
		}
		if slices.Contains(all[i+1:], name) {
			t.Errorf("All() lists %q twice", name)
		}
	}
	if Known("agy") || Known("") {
		t.Error("Known accepts a name outside the roster")
	}
	all[0], External()[0] = "mutated", "mutated"
	if All()[0] != Claude || External()[0] != Codex {
		t.Error("All or External shares state with a caller's slice")
	}
	if ext := External(); !slices.Equal(ext, All()[1:]) || slices.Contains(ext, Claude) {
		t.Errorf("External() = %v, want All() minus claude in order", ext)
	}
}

func TestBinary(t *testing.T) {
	for _, name := range All() {
		want := name
		if name == Gemini {
			want = "agy"
		}
		if got := Binary(name); got != want {
			t.Errorf("Binary(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestNativeEffort(t *testing.T) {
	// Rows are minimal, low, medium, high, xhigh, max, bogus; cells are the native value ("" = refused).
	table := map[string][7]string{
		Claude:   {"low", "low", "medium", "high", "xhigh", "max", ""},
		Codex:    {"minimal", "low", "medium", "high", "xhigh", "xhigh", ""},
		Copilot:  {"minimal", "low", "medium", "high", "xhigh", "max", ""},
		Gemini:   {"low", "low", "medium", "high", "xhigh", "max", ""},
		OpenCode: {"minimal", "low", "medium", "high", "xhigh", "max", ""},
		Pi:       {"minimal", "low", "medium", "high", "xhigh", "max", ""},
		"agy":    {},
	}
	for agent, row := range table {
		for i, e := range append(slices.Clone(efforts), "bogus") {
			native, clamped := NativeEffort(agent, e)
			if native != row[i] || clamped != (native != "" && native != string(e)) {
				t.Errorf("NativeEffort(%s, %s) = (%q, %v), want %q", agent, e, native, clamped, row[i])
			}
		}
	}
}

func TestEffortNames(t *testing.T) {
	if got := EffortNames(); !slices.Equal(got, []string{"minimal", "low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("EffortNames() = %v", got)
	}
}

func TestNativeEfforts(t *testing.T) {
	want := map[string][]string{
		Claude:   EffortNames()[1:],
		Codex:    {"minimal", "low", "medium", "high", "xhigh"},
		Copilot:  nil,
		Gemini:   EffortNames()[1:],
		OpenCode: nil,
		Pi:       {"minimal", "low", "medium", "high", "xhigh", "max"},
	}
	for _, agent := range All() {
		if got := NativeEfforts(agent); !slices.Equal(got, want[agent]) || (got == nil) != (want[agent] == nil) {
			t.Errorf("NativeEfforts(%s) = %v, want %v", agent, got, want[agent])
		}
	}
}

func TestSplitNativeEffort(t *testing.T) {
	for _, c := range []struct {
		model, base, effort string
		ok                  bool
	}{
		{"gemini-4-hydrogen-low", "gemini-4-hydrogen", "low", true},
		{"gemini-4-hydrogen-medium", "gemini-4-hydrogen", "medium", true},
		{"gemini-4-hydrogen-high", "gemini-4-hydrogen", "high", true},
		{"gemini-4-hydrogen-xhigh", "gemini-4-hydrogen", "xhigh", true},
		{"gemini-4-hydrogen-max", "gemini-4-hydrogen", "max", true},
		{"Gemini-4-Hydrogen-high", "Gemini-4-Hydrogen", "high", true},
		{"-high", "", "high", true},
		{"--high", "-", "high", true},
		{"model-high-high", "model-high", "high", true},
		{"gemini-4-hydrogen-minimal", "gemini-4-hydrogen", "minimal", false},
		{"gemini-4-hydrogen-HIGH", "gemini-4-hydrogen", "HIGH", false},
		{"gemini-4-hydrogen-high[1m]", "gemini-4-hydrogen", "high[1m]", false},
		{"gemini-4-hydrogen-", "gemini-4-hydrogen", "", false},
		{"high", "high", "", false},
		{"", "", "", false},
	} {
		base, effort, ok := SplitNativeEffort(c.model)
		if base != c.base || effort != c.effort || ok != c.ok {
			t.Errorf("SplitNativeEffort(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.model, base, effort, ok, c.base, c.effort, c.ok)
		}
	}
}
