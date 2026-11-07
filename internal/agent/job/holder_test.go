package job

import (
	"context"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

type noLauncher struct{ agent.Launcher }

func TestRunHolderRestoresLauncher(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var got *agent.Record
	orig := serveLive
	serveLive = func(_ context.Context, rec *agent.Record) error { got = rec; return nil }
	t.Cleanup(func() { serveLive = orig })

	rec := &agent.Record{ID: "live-holder-test"}
	if code := RunHolder(rec, noLauncher{}); code != 0 {
		t.Fatalf("RunHolder = %d, want 0", code)
	}
	if got == nil || got.Request.Launcher == nil {
		t.Fatal("ServeLive got a record with no Launcher (json:\"-\" field lost on reload)")
	}
}
