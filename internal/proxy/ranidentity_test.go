package proxy

import (
	"os"
	"testing"
)

func TestRanIdentityRoundTrip(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	WriteRanIdentity("d1", "sp")
	if got := ReadRanIdentity("d1"); got != "sp" {
		t.Fatalf("got %q, want sp", got)
	}
	if fi, err := os.Stat(RanIdentityPath("d1")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", fi, err)
	}
	WriteRanIdentity("d1", "user")
	WriteRanIdentity("a/b", "sp")
	if got, bad := ReadRanIdentity("d1"), ReadRanIdentity("a/b"); got != "user" || bad != "" {
		t.Fatalf("got %q %q", got, bad)
	}
}
