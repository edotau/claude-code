package proxy

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// RanIdentitySuffix names the file beside a dispatch's record holding the identity (sp|user) the proxy served it.
const RanIdentitySuffix = ".identity"

// RanIdentityPath is the identity file of dispatch id; ids hold no separator, so it stays in the dispatch dir.
func RanIdentityPath(id string) string {
	return filepath.Join(paths.DispatchDir(), id+RanIdentitySuffix)
}

// WriteRanIdentity records identity (never a credential) for dispatch id, 0600; best effort, an unchanged value is kept.
func WriteRanIdentity(id, identity string) {
	if !ValidDispatchID(id) || ReadRanIdentity(id) == identity {
		return
	}
	if _, err := paths.EnsureDispatchDir(); err != nil {
		return
	}
	_ = os.WriteFile(RanIdentityPath(id), []byte(identity), 0o600)
}

// ReadRanIdentity is the recorded identity of dispatch id, "" when none or not sp or user.
func ReadRanIdentity(id string) string {
	if !ValidDispatchID(id) {
		return ""
	}
	b, err := os.ReadFile(RanIdentityPath(id))
	if v := strings.TrimSpace(string(b)); err == nil && (v == "sp" || v == "user") {
		return v
	}
	return ""
}
