package render

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A failed later write must restore every file the plan already changed (hooks.json + config.toml pairs).
func TestApplyRollsBackOnFailure(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "hooks.json")
	created := filepath.Join(dir, "new.json")
	if err := os.WriteFile(existing, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	p := &RenderPlan{writes: []stagedWrite{
		{existing, func() error { return os.WriteFile(existing, []byte("new"), 0o600) }},
		{created, func() error { return os.WriteFile(created, []byte("x"), 0o600) }},
		{filepath.Join(dir, "config.toml"), func() error { return boom }},
	}}
	if err := p.Apply(); !errors.Is(err, boom) {
		t.Fatalf("Apply = %v, want boom", err)
	}
	if got, _ := os.ReadFile(existing); string(got) != "old" {
		t.Fatalf("hooks.json = %q, want restored %q", got, "old")
	}
	if _, err := os.Stat(created); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new.json should be removed after rollback, stat err = %v", err)
	}
}
