package hooks

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

// safetyScanDecision distinguishes a Claude ask from a silent allow at exit zero.
func safetyScanDecision(t *testing.T, payload io.Reader) (Decision, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runDirect("safety-scan", payload, &out, &errb)
	if code == ExitBlock {
		return Block, errb.String()
	}
	if code != ExitProceed {
		t.Fatalf("SafetyScan exit = %d, want 0 or 2", code)
	}
	if out.Len() > 0 {
		var doc struct {
			Output struct {
				Decision string `json:"permissionDecision"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("SafetyScan stdout is not JSON: %v (%s)", err, out.String())
		}
		if doc.Output.Decision == "ask" {
			return Ask, errb.String()
		}
	}
	return Allow, errb.String()
}
