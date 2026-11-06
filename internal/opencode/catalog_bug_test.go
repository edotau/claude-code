package opencode

import (
	"strings"
	"testing"
)

func TestParseMenuImportLongLine(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	label := strings.Repeat("long-label", 8<<10)
	got, err := ParseMenuImport([]byte("databricks-kimi-k3," + label + ",responses\n"))
	if err != nil {
		t.Fatalf("valid menu line over 64 KiB: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("parsed %d models, want 1", len(got))
	}
	if got[0].ID != "kimi-k3" || got[0].Label != label || !got[0].Responses {
		t.Fatalf("parsed ID=%q, label matches=%t, responses=%t", got[0].ID, got[0].Label == label, got[0].Responses)
	}
}
