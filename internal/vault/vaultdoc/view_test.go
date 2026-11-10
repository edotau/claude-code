package vaultdoc

import (
	"testing"

	"github.com/edotau/claude-code/internal/network/tailscale"
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
