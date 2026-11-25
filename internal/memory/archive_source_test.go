package memory

import "testing"

func TestArchiveSource(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		wantSrc string
		wantOK  bool
	}{
		{"month", "conventions-2026-10.md", "conventions", true},
		{"dream", "decisionLog-2026-11-20-dream.md", "decisionLog", true},
		{"trim", "progress-2026-11-20-trim.md", "progress", true},
		{"focus", "activeContext-2026-11-20-focus.md", "activeContext", true},
		{"repo slug", "decisionLog-2026-11-19-sample-repo.md", "decisionLog", true},
		{"month repo slug", "sessionHistory-2026-08-digitalbtd-rapid.md", "sessionHistory", true},
		{"month one word", "sessionHistory-2026-08-digitalbtd.md", "sessionHistory", true},
		{"src archive", "conventions-archive.md", "conventions", true},
		{"merged no source", "merged-claude-code-progress.md", "", false},
		{"overflow", "2026-11-18-decisionLog-overflow.md", "decisionLog", true},
		{"overflow repo slug", "2026-11-18-sessionHistory-overflow-sample-repo.md", "sessionHistory", true},
		{"unplaceable gemini", "2026-10-21-gemini-history-snapshot.md", "", false},
		{"unplaceable harness", "2026-11-01-harness-progress-snapshot.md", "", false},
		{"invented source month", "invented-2026-10.md", "", false},
		{"invented source overflow", "2026-11-18-invented-overflow.md", "", false},
		{"invented source slug", "invented-2026-11-19-repo.md", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, ok := archiveSource(tt.file)
			if src != tt.wantSrc || ok != tt.wantOK {
				t.Errorf("archiveSource(%q) = %q, %v; want %q, %v", tt.file, src, ok, tt.wantSrc, tt.wantOK)
			}
		})
	}
}

func TestGrammarOfArchiveUsesSource(t *testing.T) {
	for name, want := range map[string]Grammar{
		"archive/2026-11-01-decisionLog-overflow.md": GrammarDated,
		"archive/conventions-2026-11-20-trim.md":     GrammarBullets,
		"archive/progress-2026-11.md":                GrammarSections,
		"archive/conventions-archive.md":             GrammarBullets,
		"archive/2026-10-21-gemini-snapshot.md":      GrammarSections,
	} {
		if got := GrammarOf(name); got != want {
			t.Errorf("GrammarOf(%q) = %v, want %v", name, got, want)
		}
	}
}
