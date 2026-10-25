package statusline

import (
	"slices"
	"testing"
)

// easeAt keeps the ends fixed, leaves exp 0/1 linear, and moves the middle toward the end colors when exp < 1.
func TestEaseAt(t *testing.T) {
	for _, exp := range []float64{0, 1, 0.5} {
		if easeAt(0, exp) != 0 || easeAt(1, exp) != 1 {
			t.Errorf("exp %v must keep the ends at 0 and 1", exp)
		}
	}
	if easeAt(0.5, 0) != 0.5 || easeAt(0.5, 1) != 0.5 {
		t.Error("exp 0 and 1 must be linear")
	}
	if got := easeAt(0.25, 0.5); got != 0.5 {
		t.Errorf("easeAt(0.25, 0.5) = %v, want 0.5", got)
	}
}

func TestGradientCells(t *testing.T) {
	type run struct{ text, kind string }
	for _, tc := range []struct {
		name   string
		order  []string
		path   bool
		before bool
		want   []run
	}{
		{
			name:  "ordered segments and separator tones",
			order: []string{"path", "provider", "model", "context", "tokens", "session"},
			path:  true,
			want: []run{
				{"~/proj ", "g"}, {"|", "sep"}, {" openrouter ", "g"}, {"|", "sep"}, {" Opus 5 [1m]", "g"},
				{" ", "g"}, {"●", "dot"}, {" ", "g"}, {"│", "sep"}, {" ", "g"},
				{"++", "filled"}, {"--", "empty"}, {" 42%", "g"},
				{" ", "g"}, {"|", "sep"}, {" ~1.2M tok", "g"}, {" | ", "sep"}, {"feature…", "g"},
			},
		},
		{
			name:   "context first with percent before",
			order:  []string{"context", "model"},
			before: true,
			want: []run{
				{" ", "g"}, {"●", "dot"}, {" ", "g"}, {"│", "sep"}, {" ", "g"},
				{"++", "filled"}, {"--", "empty"}, {" ", "g"}, {"|", "sep"}, {" Opus 5 [1m]", "g"},
				{" | ", "sep"}, {"feature…", "g"},
			},
		},
		{
			name:  "missing and disabled segments",
			order: []string{"path", "session", "duration", "unknown"},
			want:  []run{{" | ", "sep"}, {"feature…", "g"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := embeddedConfig()
			cfg.Order = tc.order
			cfg.Segments = map[string]bool{"path": tc.path, "duration": false}
			cfg.Separators = map[string]string{"major": " ● │ ", "minor": " | ", "git": " | "}
			cfg.BranchMax = 8
			cfg.Bar.Width, cfg.Bar.FilledGlyph, cfg.Bar.EmptyGlyph = 4, "+", "-"
			if tc.before {
				cfg.Bar.Percent = "before"
			} else {
				cfg.Bar.Percent = "after"
			}
			v := sample()
			v.branch = "feature/statusline"
			var want []cell
			for _, r := range tc.want {
				for _, ch := range r.text {
					want = append(want, cell{ch, r.kind})
				}
			}
			if got := gradientCells(v, cfg); !slices.Equal(got, want) {
				t.Fatalf("gradientCells = %v, want %v", got, want)
			}
		})
	}
}
