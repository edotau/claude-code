package ask

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/job"
)

// A failed job's stderr tail may carry a key; result must not echo it raw.
func TestResultExitRedactsError(t *testing.T) {
	const key = "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789ABCDEF"
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	rc := ResultExit(job.Record{Status: "error", Error: "401 bad key " + key})
	os.Stderr = orig
	w.Close()
	out, _ := io.ReadAll(r)
	if rc != 1 || strings.Contains(string(out), key) {
		t.Errorf("rc=%d stderr=%q; want 1 and no raw key", rc, out)
	}
}
