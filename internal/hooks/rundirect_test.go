package hooks

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// An oversized context on a module's own-process path is cut to the cap plus one ellipsis rune, blocks joined by a blank line.
func TestRunDirectCapsContext(t *testing.T) {
	orig := Native["memory-start"]
	Native["memory-start"] = func(*Input) Result { return Result{Context: joinContext("a", "", strings.Repeat("x", 20000))} }
	t.Cleanup(func() { Native["memory-start"] = orig })
	var out strings.Builder
	if code := runDirect("memory-start", strings.NewReader(`{"session_id":"s"}`), &out, io.Discard); code != ExitProceed {
		t.Fatalf("exit %d", code)
	}
	var doc struct {
		H struct{ AdditionalContext string } `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil || !strings.HasPrefix(doc.H.AdditionalContext, "a\n\nxx") ||
		len([]rune(doc.H.AdditionalContext)) != contextCapChars+1 {
		t.Errorf("cap: %v len=%d", err, len([]rune(doc.H.AdditionalContext)))
	}
}
