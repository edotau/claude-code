package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// withTokenExpiry stamps every row from the recorded expiry as RFC3339 UTC, and leaves the key out of the JSON when none exists.
func TestWithTokenExpiryStampsRecordedExpiryAndOmitsWhenAbsent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rows := func() []quota { return []quota{{}, {}} }

	bare, err := json.Marshal(withTokenExpiry(rows(), "dbx"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), "token_expires_at") {
		t.Fatalf("no record, yet JSON carries the key: %s", bare)
	}

	exp := time.Date(2026, 10, 31, 19, 22, 58, 0, time.UTC)
	dir := filepath.Join(paths.StateDir(), "cache", "token-expiry")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := []byte(`{"schema":1,"exp":` + strconv.FormatInt(exp.Unix(), 10) + `}`)
	if err := os.WriteFile(filepath.Join(dir, "dbx.json"), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	got := withTokenExpiry(rows(), "dbx")
	for i, q := range got {
		if q.TokenExpiresAt != "2026-10-31T19:22:58Z" {
			t.Errorf("row %d TokenExpiresAt = %q, want 2026-10-31T19:22:58Z", i, q.TokenExpiresAt)
		}
	}
	if other := withTokenExpiry(rows(), "other"); other[0].TokenExpiresAt != "" {
		t.Errorf("a profile with no record was stamped %q", other[0].TokenExpiresAt)
	}
}
