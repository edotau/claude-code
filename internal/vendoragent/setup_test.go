package vendoragent

import "testing"

// TestSetupUnknownVendor: Setup reports a single failing "vendor" check, not a panic or an empty report.
func TestSetupUnknownVendor(t *testing.T) {
	rep := Setup("nonexistent-vendor")
	if rep.OK {
		t.Error("Setup(unknown vendor).OK = true, want false")
	}
	if len(rep.Checks) != 1 || rep.Checks[0].Name != "vendor" || rep.Checks[0].State != "fail" {
		t.Errorf("Setup(unknown vendor).Checks = %+v, want one failing \"vendor\" check", rep.Checks)
	}
}

// TestSetupKnownVendorChecks: Setup always returns binary/login/route/projection, in that order, for
// a registered vendor; OK tracks binary+login only (route/projection are informational, per contract.md).
// Route's own detail text is a wave-3 concern (launch.RouteDetail) and is deliberately not asserted here.
func TestSetupKnownVendorChecks(t *testing.T) {
	for _, name := range []string{"codex", "gemini", "copilot", "opencode"} {
		rep := Setup(name)
		if rep.Vendor != name {
			t.Errorf("Setup(%q).Vendor = %q", name, rep.Vendor)
		}
		if len(rep.Checks) != 4 {
			t.Fatalf("Setup(%q).Checks = %+v, want 4 rows (binary, login, route, projection)", name, rep.Checks)
		}
		wantNames := []string{"binary", "login", "route", "projection"}
		for i, c := range rep.Checks {
			if c.Name != wantNames[i] {
				t.Errorf("Setup(%q).Checks[%d].Name = %q, want %q", name, i, c.Name, wantNames[i])
			}
			if c.State != "ok" && c.State != "fail" && c.State != "warn" {
				t.Errorf("Setup(%q).Checks[%d].State = %q, want ok|fail|warn", name, i, c.State)
			}
		}
		binOK := rep.Checks[0].State == "ok"
		loginOK := rep.Checks[1].State == "ok"
		if rep.OK != (binOK && loginOK) {
			t.Errorf("Setup(%q).OK = %v, want binary.ok(%v) && login.ok(%v)", name, rep.OK, binOK, loginOK)
		}
	}
}
