package goal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A run manifest that ended not-completed fails the goal; a missing, running or half-written one is pending.
func TestEvaluateRunManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, body, out string
		pass, pending   bool
	}{
		{"wf_ok", `{"status":"completed"}`, "completed", true, false},
		{"wf_failed", `{"status":"failed"}`, "run wf_failed ended failed", false, false},
		{"wf_blank", `{}`, "run wf_blank ended without a status", false, false},
		{"wf_none", "", "no manifest yet", false, true},
		{"wf_running", `{"status":"running"}`, "still running", false, true},
		{"wf_trunc", `{"status":"runn`, "not readable yet", false, true},
	}
	for _, c := range cases {
		if c.body != "" {
			if err := os.WriteFile(filepath.Join(dir, "workflows", c.name+".json"), []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		v := Evaluate(context.Background(), Goal{Run: c.name, SessionDir: dir})
		if v.Pass != c.pass || v.Pending != c.pending || !strings.Contains(v.Out, c.out) {
			t.Errorf("%s: %+v, want pass=%v pending=%v out~%q", c.name, v, c.pass, c.pending, c.out)
		}
	}
}
