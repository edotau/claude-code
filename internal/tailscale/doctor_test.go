package tailscale

import (
	"context"
	"strings"
	"testing"
)

func TestDoctorNoCLI(t *testing.T) {
	noCLI(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no `security`: the real Keychain must not answer
	t.Setenv("PATH", t.TempDir())
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "")
	t.Setenv("TAILSCALE_API_KEY", "")
	ok, detail := Doctor(context.Background())
	if ok {
		t.Error("Doctor() ok = true, want false (no CLI)")
	}
	if !strings.Contains(detail, "cli not found") || !strings.Contains(detail, "no admin credential") {
		t.Errorf("detail = %q", detail)
	}
}

func TestDoctorRunningAndApproved(t *testing.T) {
	stubCLI(t, catScript(t, "status_running.json"))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no `security`: the real Keychain must not answer
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "set")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_SECRET", "set")
	ok, detail := Doctor(context.Background())
	if !ok {
		t.Errorf("Doctor() ok = false, detail = %q", detail)
	}
	for _, want := range []string{"Running", "exit-node approved", "api credential TAILSCALE_OAUTH_CLIENT_ID+SECRET"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q missing %q", detail, want)
		}
	}
}

// An ID without its secret (a truncated `secrets set` name, say) is reported as incomplete, not as present.
func TestDoctorHalfOAuthCredentialIsIncomplete(t *testing.T) {
	stubCLI(t, catScript(t, "status_running.json"))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no `security`: the real Keychain must not answer
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "set")
	_, detail := Doctor(context.Background())
	if !strings.Contains(detail, "api credential incomplete") {
		t.Errorf("detail = %q, want incomplete", detail)
	}
}

func TestDoctorNeverLogsCredentialValue(t *testing.T) {
	stubCLI(t, catScript(t, "status_running.json"))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no `security`: the real Keychain must not answer
	t.Setenv("TAILSCALE_API_KEY", "tskey-api-should-never-appear")
	_, detail := Doctor(context.Background())
	if strings.Contains(detail, "tskey-api-should-never-appear") {
		t.Errorf("detail leaked the credential value: %q", detail)
	}
}
