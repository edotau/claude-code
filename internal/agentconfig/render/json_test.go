package render

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitJSONObject(t *testing.T) {
	cases := []struct {
		name, content     string
		missing, noMutate bool
		unreadable        bool
		wantFile          string
		wantNote          string
		wantChanged       bool
	}{
		{name: "missing", missing: true, wantFile: "{\n  \"k\": \"v\"\n}\n", wantChanged: true},
		{name: "object keeps other keys", content: `{"a":1}`, wantFile: "{\n  \"a\": 1,\n  \"k\": \"v\"\n}\n", wantChanged: true},
		{name: "null", content: "null", wantFile: "null", wantNote: "not an object"},
		{name: "array", content: "[1]", wantFile: "[1]", wantNote: "not plain JSON"},
		{name: "bad json", content: "{bad", wantFile: "{bad", wantNote: "not plain JSON"},
		{name: "unreadable", content: `{"a":1}`, unreadable: true, wantFile: `{"a":1}`, wantNote: "cannot read"},
		{name: "mutate false", content: `{"a":1}`, noMutate: true, wantFile: `{"a":1}`},
		{name: "mutate false on missing writes nothing", missing: true, noMutate: true, wantFile: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(os.Getenv("HOME"), ".claude"))
			path := filepath.Join(t.TempDir(), "settings.json")
			if !c.missing {
				mode := os.FileMode(0o600)
				if c.unreadable {
					mode = 0
				}
				if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			var plan RenderPlan
			changed, err := CommitJSONObject(path, RenderOptions{}.Stage(&plan), &out, "x-settings", func(obj map[string]any, _ bool) bool {
				obj["k"] = "v"
				return !c.noMutate
			})
			if err == nil {
				err = plan.Apply()
			}
			if err != nil {
				t.Fatal(err)
			}
			if changed != c.wantChanged {
				t.Errorf("changed = %v, want %v", changed, c.wantChanged)
			}
			os.Chmod(path, 0o600)
			got, _ := os.ReadFile(path)
			if string(got) != c.wantFile {
				t.Errorf("file = %q, want %q", got, c.wantFile)
			}
			if want := "x-settings: skipped: "; c.wantNote != "" && !strings.Contains(out.String(), want) ||
				!strings.Contains(out.String(), c.wantNote) {
				t.Errorf("note = %q, want %q + %q", out.String(), want, c.wantNote)
			}
		})
	}
}
