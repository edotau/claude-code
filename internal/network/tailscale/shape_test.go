package tailscale

import (
	"strings"
	"testing"
)

// ValidCredential: each credential name's documented shape, plus the two real failures seen live.
func TestValidCredential(t *testing.T) {
	cases := []struct {
		name    string
		envName string
		value   string
		wantErr bool
	}{
		{"unknown name always valid", "TAILSCALE_SOMETHING_ELSE", "anything at all", false},

		{"client id valid shape", "TAILSCALE_OAUTH_CLIENT_ID", "kJaxHLLXzN11CNTRL", false},
		{"client id too short", "TAILSCALE_OAUTH_CLIENT_ID", "kshort1", true},
		{"client id too long", "TAILSCALE_OAUTH_CLIENT_ID", "k" + strings.Repeat("a", 41), true},
		{"client id not starting with k", "TAILSCALE_OAUTH_CLIENT_ID", "jaxHLLXzN11CNTRLx", true},
		{"client id non-alnum", "TAILSCALE_OAUTH_CLIENT_ID", "kJaxHLLX-N11CNTRL", true},
		{"client id whitespace", "TAILSCALE_OAUTH_CLIENT_ID", "kJaxHLLX N11CNTRL", true},
		// real failure seen live: a full pbpaste'd shell pipeline, not a key.
		{"client id real failure: pasted shell pipeline", "TAILSCALE_OAUTH_CLIENT_ID",
			"! pbpaste | claude-code vault set TAILSCALE_OAUTH_CLIENT_ID", true},

		{"client secret valid shape", "TAILSCALE_OAUTH_CLIENT_SECRET", "tskey-client-abc123XYZ", false},
		{"client secret wrong prefix", "TAILSCALE_OAUTH_CLIENT_SECRET", "not-a-secret", true},
		{"client secret whitespace", "TAILSCALE_OAUTH_CLIENT_SECRET", "tskey-client-abc 123", true},
		// real failure seen live: an auth key pasted into the client-secret slot.
		{"client secret real failure: auth key in secret slot", "TAILSCALE_OAUTH_CLIENT_SECRET",
			"tskey-auth-kXXXXXCNTRL-abcdefghijklmnopqrstuvwxyz", true},

		{"api key valid shape", "TAILSCALE_API_KEY", "tskey-api-abc123XYZ", false},
		{"api key wrong prefix", "TAILSCALE_API_KEY", "tskey-auth-abc123XYZ", true},
		{"api key whitespace", "TAILSCALE_API_KEY", "tskey-api-abc 123", true},

		{"authkey valid shape", "TAILSCALE_AUTHKEY", "tskey-auth-abc123XYZ", false},
		{"authkey wrong prefix", "TAILSCALE_AUTHKEY", "tskey-api-abc123XYZ", true},
		{"authkey whitespace", "TAILSCALE_AUTHKEY", "tskey-auth-abc\t123", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidCredential(c.envName, c.value)
			if (err != nil) != c.wantErr {
				t.Fatalf("ValidCredential(%q, %q) = %v, wantErr %v", c.envName, c.value, err, c.wantErr)
			}
			if err == nil {
				return
			}
			// Error text may name the tskey-<kind>- prefix it looks like, but never the random tail.
			msg := err.Error()
			tail := strings.TrimPrefix(c.value, "tskey-client-")
			tail = strings.TrimPrefix(tail, "tskey-api-")
			tail = strings.TrimPrefix(tail, "tskey-auth-")
			if tail != c.value && tail != "" && strings.Contains(msg, tail) {
				t.Errorf("error %q leaks the secret's random tail %q", msg, tail)
			}
		})
	}
}

// The error text never contains the full pasted value for the two real failures seen live.
func TestValidCredentialNeverEchoesFullValue(t *testing.T) {
	cases := []struct {
		name, value string
	}{
		{"TAILSCALE_OAUTH_CLIENT_SECRET", "tskey-auth-realrandomtail1234567890"},
		{"TAILSCALE_OAUTH_CLIENT_ID", "! pbpaste | claude-code vault set TAILSCALE_OAUTH_CLIENT_ID"},
	}
	for _, c := range cases {
		err := ValidCredential(c.name, c.value)
		if err == nil {
			t.Fatalf("ValidCredential(%q, %q) = nil, want an error", c.name, c.value)
		}
		if strings.Contains(err.Error(), c.value) {
			t.Errorf("%s: error %q echoes the full value", c.name, err.Error())
		}
	}
}
