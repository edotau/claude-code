package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type tailscaleTestTransport func(*http.Request) (*http.Response, error)

func (f tailscaleTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// fakeTailscaleOnPATH puts a `tailscale` script on PATH answering `status --json` per state.
func fakeTailscaleOnPATH(t *testing.T, backendState string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = status ]; then /bin/cat <<EOF\n" +
		`{"BackendState":"` + backendState + `","Self":{"DNSName":"test-mac.tailxxxxx.ts.net.","AllowedIPs":[]},"Peer":{}}` +
		"\nEOF\nfi\n"
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// fakeTailscaleUpOnPATH puts a `tailscale` script on PATH that answers `status --json` as Stopped and
// records the argv of any `up` invocation into argvFile — the seam item 7's tests drive to prove the
// auth key never rides on argv.
func fakeTailscaleUpOnPATH(t *testing.T, argvFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = status ]; then /bin/cat <<EOF\n" +
		`{"BackendState":"Stopped","Self":{"DNSName":"test-mac.tailxxxxx.ts.net.","AllowedIPs":[]},"Peer":{}}` +
		"\nEOF\nfi\n" +
		"if [ \"$1\" = up ]; then printf '%s\\n' \"$@\" > " + shQuote(argvFile) + "; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func hermeticTailscaleSecrets(t *testing.T) {
	t.Helper()
	vaulttest.Use(t)
	for _, n := range []string{
		"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET", "TAILSCALE_API_KEY", "TAILSCALE_AUTHKEY",
	} {
		t.Setenv(n, "")
	}
}

func TestTailscaleStatusNoCLI(t *testing.T) {
	hermeticTailscaleSecrets(t)
	t.Setenv("PATH", t.TempDir())
	code, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale(nil) })
	if code == 0 {
		t.Errorf("status with no CLI on PATH: code = 0, want nonzero; stderr=%q", stderr)
	}
}

func TestTailscaleStatusRunningTextAndJSON(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, _ := captureOutput(t, func() int { return cmdNetworkTailscale(nil) })
	if code != 0 {
		t.Fatalf("status exit %d", code)
	}
	if !strings.Contains(out, "test-mac.tailxxxxx.ts.net.") {
		t.Errorf("status text output missing node name:\n%s", out)
	}

	code, out, _ = captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--format", "json"}) })
	if code != 0 || !strings.Contains(out, "test-mac.tailxxxxx.ts.net.") {
		t.Errorf("status --format json: code=%d out=%q", code, out)
	}
}

func TestTailscaleConnectWithoutKeyAlreadyRunningExitsZero(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, _ := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--connect"}) })
	if code != 0 {
		t.Errorf("connect (already running, no key) exit %d, want 0", code)
	}
	if !strings.Contains(out, "already connected") {
		t.Errorf("connect output = %q, want it to say already connected", out)
	}
}

func TestTailscaleConnectWithoutKeyNotRunningExitsTwo(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Stopped")
	code, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--connect"}) })
	if code != 2 {
		t.Errorf("connect (not running, no key) exit %d, want 2; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "configure") && !strings.Contains(stderr, "auth-key") {
		t.Errorf("stderr = %q, want a configure/--auth-key hint", stderr)
	}
}

func TestTailscaleConnectUsesVaultAuthKey(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Stopped")
	if err := secrets.Store("TAILSCALE_AUTHKEY", "tskey-auth-fake"); err != nil {
		t.Fatal(err)
	}
	_, out, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--connect"}) })
	if strings.Contains(out+stderr, "no auth key given") {
		t.Errorf("connect did not use the vault auth key: stdout=%q stderr=%q", out, stderr)
	}
}

// TestTailscaleConnectAuthKeyDashReadsStdin: `--connect --auth-key -` reads the key from stdin and the
// underlying `tailscale up` never sees it on argv (it goes through tailscale.Connect's --auth-key=file:).
func TestTailscaleConnectAuthKeyDashReadsStdin(t *testing.T) {
	hermeticTailscaleSecrets(t)
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	fakeTailscaleUpOnPATH(t, argvFile)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("tskey-auth-fromstdin\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	code, out, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--connect", "--auth-key", "-"}) })
	if code != 0 {
		t.Fatalf("connect --auth-key - exit %d; out=%q stderr=%q", code, out, stderr)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("tailscale up was not invoked: %v", err)
	}
	if strings.Contains(string(argv), "tskey-") {
		t.Fatalf("auth key leaked into up argv: %q", argv)
	}
	if !strings.Contains(string(argv), "--auth-key=file:") {
		t.Fatalf("up argv = %q, want --auth-key=file:<path>", argv)
	}
}

// TestTailscaleConnectLiteralAuthKeyWarns: a literal (non "-") --auth-key still connects, but warns
// that it is visible in shell history and `ps`; the key still never reaches `tailscale up`'s argv —
// Connect always routes it through --auth-key=file:.
func TestTailscaleConnectLiteralAuthKeyWarns(t *testing.T) {
	hermeticTailscaleSecrets(t)
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	fakeTailscaleUpOnPATH(t, argvFile)

	code, _, stderr := captureOutput(t, func() int {
		return cmdNetworkTailscale([]string{"--connect", "--auth-key", "tskey-auth-literal"})
	})
	if code != 0 {
		t.Fatalf("connect exit %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "visible in shell") {
		t.Errorf("stderr = %q, want a warning about a literal --auth-key", stderr)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("tailscale up was not invoked: %v", err)
	}
	if strings.Contains(string(argv), "tskey-") {
		t.Fatalf("literal auth key still leaked into up argv: %q", argv)
	}
}

func TestTailscaleKeySaveKeepsStdoutPipeable(t *testing.T) {
	hermeticTailscaleSecrets(t)
	for _, save := range []bool{false, true} {
		name := "without_save"
		if save {
			name = "with_save"
		}
		t.Run(name, func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			t.Setenv("TAILSCALE_API_KEY", "test-api-key")
			const key = "tskey-auth-xyz"
			// Reuse the CreateAuthKey response pattern from internal/network/tailscale/api_test.go.
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v2/tailnet/-/keys" {
					t.Errorf("unexpected admin API request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusNotFound)
					return
				}
				if err := json.NewEncoder(w).Encode(tailscale.AuthKey{ID: "k1", Key: key}); err != nil {
					t.Error(err)
				}
			}))
			defer ts.Close()
			originalClient := http.DefaultClient
			t.Cleanup(func() { http.DefaultClient = originalClient })
			http.DefaultClient = &http.Client{Transport: tailscaleTestTransport(func(r *http.Request) (*http.Response, error) {
				req := r.Clone(r.Context())
				req.URL.Scheme = "http"
				req.URL.Host = strings.TrimPrefix(ts.URL, "http://")
				return ts.Client().Do(req)
			})}
			args := []string{"--key"}
			if save {
				args = append(args, "--save")
			}
			code, out, stderr := captureOutput(t, func() int { return cmdNetworkTailscale(args) })
			if code != 0 {
				t.Fatalf("key exit %d, want 0; stderr=%q", code, stderr)
			}
			if out != key+"\n" {
				t.Errorf("stdout = %q, want only %q", out, key+"\n")
			}
			stored, source, ok := secrets.Get("TAILSCALE_AUTHKEY")
			if save {
				if !ok || stored != key || source != "vault" {
					t.Errorf("saved key = %q, source=%q, found=%v", stored, source, ok)
				}
				want := "saved TAILSCALE_AUTHKEY to the vault (" + secrets.Redact(key) + ")\n"
				if stderr != want {
					t.Errorf("stderr = %q, want %q", stderr, want)
				}
			} else {
				if ok {
					t.Error("key without --save must not store TAILSCALE_AUTHKEY")
				}
				if stderr != "" {
					t.Errorf("key without --save stderr = %q, want empty", stderr)
				}
			}
		})
	}
}

func TestTailscaleKeyWithoutCredentialFails(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--key"}) })
	if code == 0 {
		t.Fatalf("key with no admin credential: exit 0, want nonzero; stdout=%q", out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("key must print nothing to stdout on failure, got %q", out)
	}
	if !strings.Contains(stderr, "credential") {
		t.Errorf("stderr = %q, want it to mention the missing credential", stderr)
	}
}

func TestTailscaleExitNodeApproveWithoutCredentialPrintsAdminURL(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, _ := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--exit-node", "approve"}) })
	if code != 2 {
		t.Errorf("exit-node --approve (no credential) exit %d, want 2", code)
	}
	if !strings.Contains(out, "login.tailscale.com/admin/machines") {
		t.Errorf("output = %q, want the admin console URL", out)
	}
}

func TestNetworkTailscaleModeParsing(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"two modes", []string{"--machines", "--connect"}, "mutually exclusive"},
		{"key and exit-node", []string{"--key", "--exit-node", "on"}, "mutually exclusive"},
		{"empty exit-node value", []string{"--exit-node", ""}, `must be on, off or approve, got ""`},
		{"machines with empty exit-node", []string{"--machines", "--exit-node", ""}, "mutually exclusive"},
		{"bad exit-node value", []string{"--exit-node", "sideways"}, "must be on, off or approve"},
		{"key flag with connect", []string{"--connect", "--expiry", "1d"}, "--expiry belongs to --key"},
		{"key flag with no mode", []string{"--expiry", "1d"}, "--expiry belongs to --key"},
		{"connect flag with key", []string{"--key", "--auth-key", "x"}, "belongs to --connect"},
		{"advertise-exit with no mode", []string{"--advertise-exit"}, "belongs to --connect"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			fakeTailscaleOnPATH(t, "Running")
			code, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale(tc.args) })
			if code != 2 {
				t.Errorf("exit %d, want 2; stderr=%q", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q lacks %q", stderr, tc.want)
			}
		})
	}
}

func TestNetworkTailscaleExitNodeValuesAccepted(t *testing.T) {
	for _, v := range []string{"on", "off"} {
		t.Run(v, func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			fakeTailscaleOnPATH(t, "Running")
			_, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--exit-node", v}) })
			if strings.Contains(stderr, "must be on, off or approve") || strings.Contains(stderr, "mutually exclusive") {
				t.Errorf("--exit-node %s rejected by validation: %q", v, stderr)
			}
		})
	}
}

func TestNetworkTailscaleMachinesIsAMode(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	_, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--machines"}) })
	if strings.Contains(stderr, "belongs to") || strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("--machines alone is a valid mode: %q", stderr)
	}
}

func TestNetworkTailscaleRouteEmptyValueExitsTwo(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--route", ""}) })
	if code != 2 {
		t.Errorf("--route with empty value: exit %d, want 2; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "requires a peer name") {
		t.Errorf("stderr = %q, want a message about requiring a peer name", stderr)
	}
}

func TestNetworkTailscaleRouteValueStartingWithDashExitsTwo(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--route", "-invalid"}) })
	if code != 2 {
		t.Errorf("--route with dash-value: exit %d, want 2; stderr=%q", code, stderr)
	}
}

func TestNetworkTailscaleRouteMutualExclusivity(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"route and machines", []string{"--route", "off", "--machines"}},
		{"route and connect", []string{"--route", "peer", "--connect"}},
		{"route and key", []string{"--route", "off", "--key"}},
		{"route and exit-node", []string{"--route", "peer", "--exit-node", "on"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			fakeTailscaleOnPATH(t, "Running")
			code, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale(tc.args) })
			if code != 2 {
				t.Errorf("exit %d, want 2; stderr=%q", code, stderr)
			}
			if !strings.Contains(stderr, "mutually exclusive") {
				t.Errorf("stderr = %q, want it to mention mutual exclusion", stderr)
			}
		})
	}
}

func TestNetworkTailscaleRouteRunsAndReports(t *testing.T) {
	for args, want := range map[string]string{"off": "exit node cleared", "my-peer": "routing via my-peer"} {
		t.Run(args, func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			fakeTailscaleOnPATH(t, "Running")
			code, stdout, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--route", args}) })
			if code != 0 || !strings.Contains(stdout, want) {
				t.Errorf("--route %s: exit %d stdout %q stderr %q, want 0 and %q", args, code, stdout, stderr, want)
			}
		})
	}
}

func TestNetworkTailscaleSubnetParsing(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"empty value", []string{"--subnet", ""}, `must be a CIDR list, approve or off, got ""`},
		{"not a cidr", []string{"--subnet", "sideways"}, "must be a CIDR list, approve or off"},
		{"bad second cidr", []string{"--subnet", "192.168.1.0/24,nope"}, "must be a CIDR list, approve or off"},
		{"subnet and exit-node", []string{"--subnet", "192.168.1.0/24", "--exit-node", "on"}, "mutually exclusive"},
		{"subnet and route", []string{"--subnet", "approve", "--route", "peer"}, "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			fakeTailscaleOnPATH(t, "Running")
			code, _, stderr := captureOutput(t, func() int { return cmdNetworkTailscale(tc.args) })
			if code != 2 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d stderr %q, want 2 and %q", code, stderr, tc.want)
			}
		})
	}
}

func TestNetworkTailscaleSubnetApproveWithoutCredentialPrintsAdminURL(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, _ := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--subnet", "approve"}) })
	if code != 2 || !strings.Contains(out, "login.tailscale.com/admin/machines") {
		t.Errorf("--subnet approve (no credential): exit %d stdout %q, want 2 and the admin URL", code, out)
	}
}

func TestNetworkTailscaleSubnetAdvertiseAndOffReport(t *testing.T) {
	for args, want := range map[string]string{
		"192.168.1.0/24,10.9.0.0/16": "advertising 192.168.1.0/24, 10.9.0.0/16",
		"off":                        "subnet routes withdrawn",
	} {
		t.Run(args, func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			fakeTailscaleOnPATH(t, "Running")
			code, stdout, stderr := captureOutput(t, func() int { return cmdNetworkTailscale([]string{"--subnet", args}) })
			if code != 0 || !strings.Contains(stdout, want) {
				t.Errorf("--subnet %s: exit %d stdout %q stderr %q, want 0 and %q", args, code, stdout, stderr, want)
			}
		})
	}
}
