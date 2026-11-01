package cli

import (
	"context"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

func TestRunLiveHolderRestoresLauncher(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var got *agent.Record
	orig := serveLive
	serveLive = func(_ context.Context, rec *agent.Record, _ func(string)) error { got = rec; return nil }
	t.Cleanup(func() { serveLive = orig })

	rec := &agent.Record{ID: "live-holder-test"}
	if rec.Request.Launcher != nil {
		t.Fatal("fixture must start without a launcher")
	}
	if code := runLiveHolder(rec); code != 0 {
		t.Fatalf("runLiveHolder = %d, want 0", code)
	}
	if got == nil || got.Request.Launcher == nil {
		t.Fatal("ServeLive got a record with no Launcher (json:\"-\" field lost on reload)")
	}
}
