package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// seed writes a file under state/harness and returns its path.
func seedHarness(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(paths.StateDir(), name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFormatsDoctorAndInstallStepSpan(t *testing.T) {
	hermetic(t, "")
	quota := seedHarness(t, "quota.json", `{"acct":{"until":"2099-01-01T00:00:00Z"}}`)
	seedHarness(t, "agentversion.json", `{"claude":"1.0.0"}`) // unversioned cache: a miss, never stale
	seedHarness(t, "zz-stray.json", `{}`)

	formats := findCheck(formatsDoctorChecks(), "formats")
	if formats == nil || !formats.OK || strings.Contains(formats.Detail, "stale") { // quota stays a bare map: an old daemon reads it
		t.Fatalf("formats row = %+v, want current", formats)
	}
	layout := findCheck(formatsDoctorChecks(), "state layout")
	if layout == nil || !strings.Contains(layout.Detail, "zz-stray.json") {
		t.Fatalf("state layout row = %+v, want stray file listed", layout)
	}
	if strings.Contains(layout.Detail, "quota.json") {
		t.Errorf("registered file flagged in layout: %q", layout.Detail)
	}

	before, _ := os.ReadFile(quota)
	var out bytes.Buffer
	installFormats(&out, false)
	if after, _ := os.ReadFile(quota); !bytes.Equal(before, after) {
		t.Fatalf("install rewrote quota.json: %s", after)
	}
	if _, err := os.Stat(schemas.LedgerPath()); err == nil {
		t.Error("install wrote the ledger for a bare-map quota.json")
	}
}
