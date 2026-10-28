package providers

import (
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func orderDoc(names ...string) []byte {
	rows := ""
	for i, n := range names {
		if i > 0 {
			rows += ","
		}
		rows += `"` + n + `":{"preset":"databricks","host":"https://` + n + `.cloud.databricks.com"}`
	}
	return []byte(`{"providers":{"databricks":{` + rows + `}}}`)
}

func TestProfilesOfReorderedDocReordersOutput(t *testing.T) {
	vaulttest.UseKeyring(t)
	reg, err := Parse(orderDoc("dev", "qa", "prod"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reg.ProfilesOf("databricks"), []string{"dev", "qa", "prod"}; !slices.Equal(got, want) {
		t.Errorf("ProfilesOf = %v, want %v", got, want)
	}
	if got, want := DatabricksProfiles(reg), []string{"dev", "qa", "prod"}; !slices.Equal(got, want) {
		t.Errorf("DatabricksProfiles = %v, want %v", got, want)
	}
	// negative control: the same rows declared in another order come out in that order, not sorted
	re, err := Parse(orderDoc("prod", "dev", "qa"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := re.ProfilesOf("databricks"), []string{"prod", "dev", "qa"}; !slices.Equal(got, want) {
		t.Errorf("reordered ProfilesOf = %v, want %v", got, want)
	}
}

func TestProfilesOfOverlayRowsFollowDefaults(t *testing.T) {
	vaulttest.UseKeyring(t)
	reg, err := Parse(orderDoc("qa", "dev"), orderDoc("zeta", "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reg.ProfilesOf("databricks"), []string{"qa", "dev", "zeta", "alpha"}; !slices.Equal(got, want) {
		t.Errorf("ProfilesOf = %v, want defaults then overlay rows in declared order %v", got, want)
	}
}

func TestPinIgnoresProcessEnv(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv(PinCopilotModel, "from-env")
	if got := Pin(PinCopilotModel); got != "" {
		t.Errorf("Pin(env only) = %q, want empty: pins are vault-only", got)
	}
	if err := SetPins(map[string]string{PinCopilotModel: "from-vault"}); err != nil {
		t.Fatal(err)
	}
	if got := Pin(PinCopilotModel); got != "from-vault" {
		t.Errorf("Pin(vault+env) = %q, want from-vault", got)
	}
}

func TestPinProfileIsNotTheSessionMarker(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv(EnvSessionProfile, "qa")
	if got := Pin(PinProfile); got != "" {
		t.Errorf("Pin(PinProfile) = %q, want empty: the session marker is read by SessionProfile only", got)
	}
	if got := SessionProfile(); got != "qa" {
		t.Errorf("SessionProfile = %q, want qa", got)
	}
}

func TestValidRouteAccountKey(t *testing.T) {
	for key, want := range map[string]bool{
		"anthropic·max": true, "github·copilot": true, "github·someuser": true, "opencode·go": true,
		"copilot:x": false, "copilot": false, "anthropic:max": false, "": false,
	} {
		if got := validRouteAccountKey(key); got != want {
			t.Errorf("validRouteAccountKey(%q) = %v, want %v", key, got, want)
		}
	}
}
