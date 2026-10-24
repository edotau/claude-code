package plan

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const driftDocRel = "skills/subagent-workflows/references/saved-workflows.md"

// Saved-workflows doc vs workflows/*.js: each side must name the other, else the verb exits 1.
func TestWorkflowLintDrift(t *testing.T) {
	const okBody = "export const meta = { name: 'x' }\nawait agent('x', { model: 'sonnet', effort: 'low' })\n"
	table := func(names ...string) string {
		s := "| Workflow | Use |\n| --- | --- |\n"
		for _, n := range names {
			s += "| `" + n + "` | does " + n + " |\n"
		}
		return s
	}
	cases := []struct {
		name    string
		scripts []string
		doc     *string
		wantRC  int
		wantOut string
		wantErr []string
	}{
		{"in sync", []string{"a", "b"}, ptr(table("a", "b")), 0, "workflow lint: 2 script(s) clean", nil},
		{"undocumented script", []string{"a", "b"}, ptr(table("a")), 1, "",
			[]string{filepath.Join("workflows", "b.js") + ": not documented in " + driftDocRel}},
		{"documented but missing", []string{"a"}, ptr(table("a", "ghost")), 1, "",
			[]string{"saved-workflows.md: ghost has no workflows/ghost.js"}},
		{"no doc skips check", []string{"a", "b"}, nil, 0, "workflow lint: 2 script(s) clean", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			wf := filepath.Join(root, "workflows")
			if err := os.Mkdir(wf, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, s := range tc.scripts {
				writeScript(t, wf, s+".js", okBody)
			}
			if tc.doc != nil {
				p := filepath.Join(root, driftDocRel)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(*tc.doc), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var out, errb bytes.Buffer
			rc := RunWorkflowLint([]string{root}, &out, &errb)
			if rc != tc.wantRC {
				t.Fatalf("rc=%d want %d; out=%q err=%q", rc, tc.wantRC, out.String(), errb.String())
			}
			if tc.wantOut != "" && !strings.Contains(out.String(), tc.wantOut) {
				t.Fatalf("stdout %q lacks %q", out.String(), tc.wantOut)
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(errb.String(), w) {
					t.Fatalf("stderr %q lacks %q", errb.String(), w)
				}
			}
			if tc.wantRC == 0 && errb.Len() != 0 {
				t.Fatalf("unexpected stderr %q", errb.String())
			}
		})
	}
}

func ptr(s string) *string { return &s }
