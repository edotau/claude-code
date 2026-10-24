package theme

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/ansi"
)

func TestLoadPalette(t *testing.T) {
	cases := []struct {
		name    string
		body    string // empty + noFile means no file
		noFile  bool
		ok      bool
		accent  ansi.RGB
		selects ansi.RGB
	}{
		{"valid", `{"Border":"#4796E4","Sel":"#2D4A7A"}`, false, true, ansi.RGB{71, 150, 228}, ansi.RGB{45, 74, 122}},
		{"comment keys ignored", `{"// note":"x","Border":"#4796E4","Sel":"#2D4A7A","//b":"#zzz"}`, false, true, ansi.RGB{71, 150, 228}, ansi.RGB{45, 74, 122}},
		{"eight digit hex", `{"Border":"#4796E4FF","Sel":"#2D4A7A80"}`, false, true, ansi.RGB{71, 150, 228}, ansi.RGB{45, 74, 122}},
		{"missing Sel", `{"Border":"#4796E4"}`, false, false, ansi.RGB{}, ansi.RGB{}},
		{"missing Border", `{"Sel":"#2D4A7A"}`, false, false, ansi.RGB{}, ansi.RGB{}},
		{"bad hex", `{"Border":"nope","Sel":"#2D4A7A"}`, false, false, ansi.RGB{}, ansi.RGB{}},
		{"bad json", `{`, false, false, ansi.RGB{}, ansi.RGB{}},
		{"missing file", "", true, false, ansi.RGB{}, ansi.RGB{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if !c.noFile {
				if err := os.WriteFile(filepath.Join(dir, "palette.json"), []byte(c.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			p, ok := LoadPalette(dir)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if ok && (p.Accent != c.accent || p.Selection != c.selects) {
				t.Errorf("palette = %+v, want accent %v selection %v", p, c.accent, c.selects)
			}
		})
	}
}
