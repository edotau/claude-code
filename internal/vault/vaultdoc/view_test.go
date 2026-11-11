package vaultdoc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestTailscaleAPIVerdict(t *testing.T) {
	cred := func(id, sec, api bool) []tailscale.CredState {
		return []tailscale.CredState{{Present: id}, {Present: sec}, {Present: api}}
	}
	for _, tc := range []struct {
		name         string
		id, sec, api bool
		want         string
	}{
		{"full oauth", true, true, false, "oauth"},
		{"full oauth plus api key", true, true, true, "oauth"},
		{"api key only", false, false, true, "api-key"},
		{"half oauth id plus api key falls back to api-key", true, false, true, "api-key"},
		{"half oauth secret plus api key falls back to api-key", false, true, true, "api-key"},
		{"half oauth id only", true, false, false, "incomplete (need both …_ID and _SECRET)"},
		{"half oauth secret only", false, true, false, "incomplete (need both …_ID and _SECRET)"},
		{"none", false, false, false, "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tailscaleAPIVerdict(cred(tc.id, tc.sec, tc.api)); got != tc.want {
				t.Errorf("tailscaleAPIVerdict(id=%v,sec=%v,api=%v) = %q, want %q", tc.id, tc.sec, tc.api, got, tc.want)
			}
		})
	}
}

func TestRecordLabelsReportsUnreadableVault(t *testing.T) {
	dir := vaulttest.UseFreshKey(t)
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state", "vault.db"), []byte("not a vault"), 0o600); err != nil {
		t.Fatal(err)
	}
	var labels map[string]string
	_, _, stderr := hosttest.CaptureOutput(t, func() int { labels = recordLabels(); return 0 })
	if len(labels) != 0 || !strings.Contains(stderr, "vault ls: reading labels:") {
		t.Fatalf("labels=%v stderr=%q, want empty map and a reading-labels warning", labels, stderr)
	}
}
