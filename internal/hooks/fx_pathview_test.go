package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/pathview"
)

func TestTrailingPunctuationKeepsRealFilename(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"report (1)", "wow!"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res := &pathview.Resolver{}
	reps := collectReports("read "+filepath.Join(dir, "wow!")+" now", res)
	if len(reps) != 1 || !strings.HasSuffix(reps[0].path, "wow!") {
		t.Fatalf("reps = %+v", reps)
	}
	reps = collectReports("see ("+filepath.Join(dir, "wow!")+")", res)
	if len(reps) != 1 || !strings.HasSuffix(reps[0].path, "wow!") {
		t.Fatalf("reps with paren = %+v", reps)
	}
}
