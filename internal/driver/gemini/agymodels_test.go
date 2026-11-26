package gemini

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestParseAgyModels(t *testing.T) {
	out := "Fetching available models...\ngemini-4-neon-high\tGemini 4 Neon (High)\nclaude-opus-5-5-low\tClaude Opus 5.5 (Low)\n"
	if got := ParseAgyModels([]byte(out)); !slices.Equal(got, []string{"gemini-4-neon-high", "claude-opus-5-5-low"}) {
		t.Errorf("ids = %v, want the two tab-separated rows", got)
	}
}

// A fake agy that outlives the shrunk timeout is killed and reported as one line.
func TestExecAgyModelsTimesOut(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := agyModelsTimeout
	agyModelsTimeout = 100 * time.Millisecond
	t.Cleanup(func() { agyModelsTimeout = old })
	if _, err := execAgyModels(context.Background(), bin); err == nil || err.Error() != "agy models timed out" {
		t.Errorf("err = %v, want agy models timed out", err)
	}
}
