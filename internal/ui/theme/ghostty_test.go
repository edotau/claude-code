package theme

import (
	"bytes"
	"os"
	"testing"
)

// TestMergeGhostty tests the core merge logic for Ghostty key=value config files.
func TestMergeGhostty(t *testing.T) {
	cases := []struct {
		name    string
		dest    string // initial dest content
		overlay string // overlay content
		want    string // expected merged result
	}{
		{
			name:    "simple replace one setting",
			dest:    "theme = dark\nfont-family = monospace\n",
			overlay: "theme = light\n",
			want:    "theme = light\nfont-family = monospace\n",
		},
		{
			name:    "add new key at end",
			dest:    "theme = dark\n",
			overlay: "font-size = 12\n",
			want:    "theme = dark\nfont-size = 12\n",
		},
		{
			name:    "preserve comments and blank lines in dest",
			dest:    "# config for ghostty\ntheme = dark\n# appearance settings\nfont-family = monospace\n\n",
			overlay: "theme = light\n",
			want:    "# config for ghostty\ntheme = light\n# appearance settings\nfont-family = monospace\n",
		},
		{
			name:    "replace at first occurrence, remove duplicates",
			dest:    "keybind = ctrl+a=copy\nfont = mono\nkeybind = alt+b=paste\n",
			overlay: "keybind = ctrl+c=cut\nkeybind = ctrl+v=paste\n",
			want:    "keybind = ctrl+c=cut\nkeybind = ctrl+v=paste\nfont = mono\n",
		},
		{
			name:    "spaces around = are trimmed",
			dest:    "theme   =   dark\nfont = mono\n",
			overlay: "theme = light\n",
			want:    "theme = light\nfont = mono\n",
		},
		{
			name:    "empty dest",
			dest:    "",
			overlay: "theme = light\nfont-size = 12\n",
			want:    "theme = light\nfont-size = 12\n",
		},
		{
			name:    "blank and comment lines in overlay ignored",
			dest:    "theme = dark\n",
			overlay: "# this comment is ignored\n\ntheme = light\n",
			want:    "theme = light\n",
		},
		{
			name:    "multi-value key (repeated) all replaced in order",
			dest:    "palette = color1\npalette = color2\npalette = color3\nfont = mono\n",
			overlay: "palette = new1\npalette = new2\n",
			want:    "palette = new1\npalette = new2\nfont = mono\n",
		},
		{
			name:    "dest key position preserved when key unchanged",
			dest:    "a = 1\ntheme = dark\nc = 3\n",
			overlay: "b = 2\n",
			want:    "a = 1\ntheme = dark\nc = 3\nb = 2\n",
		},
		{
			name:    "unchanged merge returns same content",
			dest:    "theme = light\n",
			overlay: "theme = light\n",
			want:    "theme = light\n",
		},
		{
			name:    "inline comments in unchanged dest lines are preserved",
			dest:    "theme = dark\nfont = mono # my font choice\n",
			overlay: "theme = light\n",
			want:    "theme = light\nfont = mono # my font choice\n",
		},
		{
			name:    "complex mix: replace, add, preserve, repeated",
			dest:    "# header\ntheme = dark\nfont = serif\n\nkeybind = a\nkeybind = b\n",
			overlay: "theme = light\ncolor = blue\nkeybind = x\nkeybind = y\n",
			want:    "# header\ntheme = light\nfont = serif\n\nkeybind = x\nkeybind = y\ncolor = blue\n",
		},
		{
			name:    "values with = and spaces preserved exactly",
			dest:    "font-features = liga=1 dlig=0\n",
			overlay: "font-features = liga=0 dlig=1\n",
			want:    "font-features = liga=0 dlig=1\n",
		},
		{
			name:    "empty overlay, dest unchanged",
			dest:    "theme = dark\n",
			overlay: "",
			want:    "theme = dark\n",
		},
		{
			name:    "empty dest, empty overlay",
			dest:    "",
			overlay: "",
			want:    "",
		},
		{
			name:    "overlay with only comments and blanks",
			dest:    "theme = dark\n",
			overlay: "# comment\n\n# another\n",
			want:    "theme = dark\n",
		},
		{
			name:    "key with empty value replaces old value",
			dest:    "key = old\n",
			overlay: "key = \n",
			want:    "key = \n",
		},
		{
			name:    "multiple keys with mixed changes",
			dest:    "a = 1\nb = 2\nc = 3\nd = 4\n",
			overlay: "a = 10\nc = 30\n",
			want:    "a = 10\nb = 2\nc = 30\nd = 4\n",
		},
		{
			name:    "keybind repeated 3 times with 2 replacements",
			dest:    "keybind = old1\nfont = serif\nkeybind = old2\nkeybind = old3\n",
			overlay: "keybind = new1\nkeybind = new2\n",
			want:    "keybind = new1\nkeybind = new2\nfont = serif\n",
		},
		{
			name:    "trailing newlines preserved",
			dest:    "theme = dark\n\n\n",
			overlay: "theme = light\n",
			want:    "theme = light\n",
		},
		{
			name:    "no final newline in overlay added to result",
			dest:    "theme = dark\n",
			overlay: "theme = light",
			want:    "theme = light\n",
		},
		{
			name:    "literal # not at line start in value is preserved",
			dest:    "opt = val#hash\n",
			overlay: "opt = new#hash\n",
			want:    "opt = new#hash\n",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeGhostty([]byte(c.dest), []byte(c.overlay))
			if !bytes.Equal(got, []byte(c.want)) {
				t.Errorf("mergeGhostty mismatch:\ngot:\n%q\nwant:\n%q", string(got), c.want)
			}
		})
	}
}

// TestMergeGhosttyIntegration tests mergeGhostty through confMergeFile to verify file I/O and backup.
func TestMergeGhosttyIntegration(t *testing.T) {
	cases := []struct {
		name          string
		dest          string // initial dest content
		overlay       string // overlay content
		dryRun        bool
		wantChanged   bool
		wantHasBackup bool
	}{
		{
			name:          "changed case produces backup when not dry-run",
			dest:          "theme = dark\n",
			overlay:       "theme = light\n",
			dryRun:        false,
			wantChanged:   true,
			wantHasBackup: true,
		},
		{
			name:          "unchanged case no backup",
			dest:          "theme = light\n",
			overlay:       "theme = light\n",
			dryRun:        false,
			wantChanged:   false,
			wantHasBackup: false,
		},
		{
			name:          "dry-run reports changed but does not backup",
			dest:          "theme = dark\n",
			overlay:       "theme = light\n",
			dryRun:        true,
			wantChanged:   true,
			wantHasBackup: false,
		},
		{
			name:          "no initial dest file creates it with no backup",
			dest:          "",
			overlay:       "theme = light\n",
			dryRun:        false,
			wantChanged:   true,
			wantHasBackup: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dest := t.TempDir() + "/config"

			// Write initial dest if provided
			if c.dest != "" {
				if err := os.WriteFile(dest, []byte(c.dest), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			// Call confMergeFile
			changed, backup, err := confMergeFile(dest, []byte(c.overlay), c.dryRun)
			if err != nil {
				t.Fatalf("confMergeFile failed: %v", err)
			}

			// Check changed signal
			if changed != c.wantChanged {
				t.Errorf("changed = %v, want %v", changed, c.wantChanged)
			}

			// Check backup presence
			hasBackup := backup != ""
			if hasBackup != c.wantHasBackup {
				t.Errorf("backup present = %v, want %v (backup=%q)", hasBackup, c.wantHasBackup, backup)
			}

			// Verify dry-run did not modify file
			if c.dryRun && c.dest != "" {
				got, _ := os.ReadFile(dest)
				if !bytes.Equal(got, []byte(c.dest)) {
					t.Error("dry-run must not modify dest file")
				}
			}
		})
	}
}
