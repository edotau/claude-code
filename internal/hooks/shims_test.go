package hooks

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// Handler is the process-shaped entry of one hook module: stdin, stdout, stderr, exit code.
type Handler func(stdin io.Reader, stdout, stderr io.Writer) int

// direct runs the named module through its own-process path (`hook <Event> --<module>`).
func direct(name string) Handler {
	return func(stdin io.Reader, stdout, stderr io.Writer) int { return runDirect(name, stdin, stdout, stderr) }
}

func nativeHandler(name string) Handler { return direct(name) }

func SafetyScan(r io.Reader, out, errw io.Writer) int { return runDirect("safety-scan", r, out, errw) }
func RTKRewrite(r io.Reader, out, errw io.Writer) int { return runDirect("rtk-rewrite", r, out, errw) }
func ReviewSimplicity(r io.Reader, out, errw io.Writer) int {
	return runDirect("review-simplicity", r, out, errw)
}
func PlanSkeleton(r io.Reader, out, errw io.Writer) int {
	return runDirect("docs-plan-skeleton", r, out, errw)
}
func PlanTick(r io.Reader, out, errw io.Writer) int { return runDirect("docs-plan-tick", r, out, errw) }
func PlanReminder(r io.Reader, out, errw io.Writer) int {
	return runDirect("docs-plan-reminder", r, out, errw)
}
func WorktreeCreate(r io.Reader, out, errw io.Writer) int {
	return runDirect("worktree-create", r, out, errw)
}
func SessionStart(r io.Reader, out io.Writer) int {
	return runDirect("memory-start", r, out, io.Discard)
}
func MemoryRecall(r io.Reader, out io.Writer) int {
	return runDirect("memory-recall", r, out, io.Discard)
}
func DocsPlacement(r io.Reader, errw io.Writer) int {
	return runDirect("docs-placement", r, io.Discard, errw)
}
func PlanGate(r io.Reader, errw io.Writer) int {
	return runDirect("docs-plan-gate", r, io.Discard, errw)
}
func PlanMirror(r io.Reader, errw io.Writer) int {
	return runDirect("docs-plan-mirror", r, io.Discard, errw)
}
func SavePlan(r io.Reader, errw io.Writer) int {
	return runDirect("docs-plan-save", r, io.Discard, errw)
}
func WorktreeRemove(r io.Reader, errw io.Writer) int {
	return runDirect("worktree-remove", r, io.Discard, errw)
}
func Stop(r io.Reader, errw io.Writer) int { return runDirect("memory-update", r, io.Discard, errw) }
func StopFormat(r io.Reader) int           { return runDirect("format-stop", r, io.Discard, io.Discard) }
func SessionUpdateEnd(r io.Reader) int     { return runDirect("memory-end", r, io.Discard, io.Discard) }
func PreCompact(r io.Reader) int           { return runDirect("memory-compact", r, io.Discard, io.Discard) }

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
