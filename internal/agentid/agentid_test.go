package agentid

import (
	"slices"
	"strings"
	"testing"

	_ "github.com/edotau/claude-code/internal/nonet" // tests refuse real DNS: mock API calls
)

func TestAll(t *testing.T) {
	all := All()
	if want := []string{"claude", "codex", "copilot", "gemini", "opencode"}; !slices.Equal(all, want) {
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
	const pass, clamp, refuse = "same", "clamp", "refuse"
	// Rows are low, medium, high, xhigh, max, minimal, bogus; cells are the outcome per the plan's table.
	table := map[string][7]string{
		Claude:   {pass, pass, pass, pass, pass, refuse, refuse},
		Codex:    {pass, pass, pass, pass, clamp, pass, refuse},
		Copilot:  {pass, pass, pass, pass, pass, refuse, refuse},
		Gemini:   {pass, pass, pass, pass, pass, refuse, refuse},
		OpenCode: {pass, pass, pass, pass, pass, refuse, refuse},
		"agy":    {refuse, refuse, refuse, refuse, refuse, refuse, refuse},
	}
	levels := append(efforts(), "minimal", "bogus")
	for agent, row := range table {
		for i, e := range levels {
			native, clamped := NativeEffort(agent, e)
			want, wantClamped := string(e), false
			switch row[i] {
			case clamp:
				want, wantClamped = string(EffortXHigh), true
			case refuse:
				want = ""
			}
			if native != want || clamped != wantClamped {
				t.Errorf("NativeEffort(%s, %s) = (%q, %v), want (%q, %v)", agent, e, native, clamped, want, wantClamped)
			}
		}
	}
}

func TestValidEffort(t *testing.T) {
	if got := EffortNames(); !slices.Equal(got, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("EffortNames() = %v", got)
	}
	for _, ok := range [][2]string{{Codex, ""}, {Codex, "max"}, {Codex, "minimal"}, {Copilot, "xhigh"}} {
		if err := ValidEffort(ok[0], ok[1]); err != nil {
			t.Errorf("ValidEffort(%s, %q) = %v, want nil", ok[0], ok[1], err)
		}
	}
	for _, bad := range [][2]string{{Claude, "minimal"}, {Gemini, "bogus"}, {Codex, "High"}, {Codex, " high"}} {
		err := ValidEffort(bad[0], bad[1])
		if err == nil || !strings.Contains(err.Error(), "low, medium, high, xhigh, max") {
			t.Errorf("ValidEffort(%s, %q) = %v, want an error naming the accepted values", bad[0], bad[1], err)
		}
	}
}

func TestNativeEfforts(t *testing.T) {
	want := map[string][]string{
		Claude:   EffortNames(),
		Codex:    {"minimal", "low", "medium", "high", "xhigh"},
		Copilot:  nil,
		Gemini:   EffortNames(),
		OpenCode: nil,
	}
	for _, agent := range All() {
		if got := NativeEfforts(agent); !slices.Equal(got, want[agent]) || (got == nil) != (want[agent] == nil) {
			t.Errorf("NativeEfforts(%s) = %v, want %v", agent, got, want[agent])
		}
	}
}
