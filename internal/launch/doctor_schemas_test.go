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
	if formats == nil || !strings.Contains(formats.Detail, "quota") || !strings.Contains(formats.Detail, "stale") {
		t.Fatalf("formats row = %+v, want quota stale", formats)
	}
	if strings.Contains(formats.Detail, "agentversion") {
		t.Errorf("unversioned cache reported stale: %q", formats.Detail)
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
	installFormats(&out, true)
	if after, _ := os.ReadFile(quota); !bytes.Equal(before, after) {
		t.Fatalf("dry-run rewrote quota.json: %s", after)
	}
	if _, err := os.Stat(schemas.LedgerPath()); err == nil {
		t.Error("dry-run wrote the ledger")
	}

	installFormats(&out, false)
	got, _ := os.ReadFile(quota)
	if !strings.Contains(string(got), `"schema":1`) {
		t.Errorf("quota.json not stamped: %s", got)
	}
	led, _ := os.ReadFile(schemas.LedgerPath())
	lines := strings.Split(strings.TrimSpace(string(led)), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"format":"quota"`) ||
		!strings.Contains(lines[0], `"from":0`) || !strings.Contains(lines[0], `"to":1`) {
		t.Errorf("ledger = %q, want one quota 0->1 line", led)
	}
	if c := findCheck(formatsDoctorChecks(), "formats"); c == nil || !c.OK || strings.Contains(c.Detail, "quota") {
		t.Errorf("formats row after migrate = %+v, want current", c)
	}
}
