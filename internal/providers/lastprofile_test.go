package providers

import (
	"testing"

	"github.com/edotau/claude-code/internal/schemas"
)

// Given a v1 file (one entry per workspace), decoding yields that entry under "claude".
func TestLastProfileMigratesV1ToClaude(t *testing.T) {
	raw := []byte(`{"schema":1,"workspaces":{"/w/a":{"profile":"prod","updated":"2026-01-02T03:04:05Z"}}}`)
	var st lastProfiles
	if _, err := schemas.Decode(lastProfileFormat, raw, &st); err != nil {
		t.Fatal(err)
	}
	if got := st.Workspaces["/w/a"]["claude"].Profile; got != "prod" || len(st.Workspaces["/w/a"]) != 1 {
		t.Errorf("v1 entry: got %+v, want claude=prod only", st.Workspaces)
	}
}
