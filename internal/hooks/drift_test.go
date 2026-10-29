package hooks

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
)

// Session start names the drifted-key count from the defaultsDrift seam, and says nothing when clean or on error.
func TestSessionStartDriftLine(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv(memory.HarvestChildEnv, "")
	orig := defaultsDrift
	t.Cleanup(func() { defaultsDrift = orig })
	cases := []struct {
		name string
		keys []string
		err  error
		want string
	}{
		{"two keys", []string{"model", "env.X"}, nil, "Settings: 2 keys differ from your defaults — claude-code configure --restore"},
		{"zero keys", nil, nil, ""},
		{"error", []string{"model"}, errors.New("boom"), ""},
	}
	for _, c := range cases {
		defaultsDrift = func() ([]string, error) { return c.keys, c.err }
		got := memoryContext(t.TempDir(), "", "", time.Now())
		if c.want == "" && strings.Contains(got, "Settings:") || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: want %q, got %q", c.name, c.want, got)
		}
	}
}
