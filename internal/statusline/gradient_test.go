package statusline

import (
	"slices"
	"testing"
)

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
