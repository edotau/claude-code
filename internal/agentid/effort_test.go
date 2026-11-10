package agentid

import (
	"slices"
	"strings"
	"testing" // tests refuse real DNS: mock API calls

	_ "github.com/edotau/claude-code/internal/nonet"
)

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
	levels := append(Efforts(), "minimal", "bogus")
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
