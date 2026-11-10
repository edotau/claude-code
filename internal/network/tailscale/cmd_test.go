package tailscale

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type tailscaleTestTransport func(*http.Request) (*http.Response, error)

func (f tailscaleTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// routeDefaultClientTo sends http.DefaultClient requests to ts, whatever host they name, until the test ends.
func routeDefaultClientTo(t *testing.T, ts *httptest.Server) {
	t.Helper()
	orig := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = orig })
	http.DefaultClient = &http.Client{Transport: tailscaleTestTransport(func(r *http.Request) (*http.Response, error) {
		req := r.Clone(r.Context())
		req.URL.Scheme, req.URL.Host = "http", strings.TrimPrefix(ts.URL, "http://")
		return ts.Client().Do(req)
	})}
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

// fakeTailscaleUpOnPATH answers `status --json` as Stopped and records `up`'s argv to prove the key is never on it.
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
		"if [ \"$1\" = up ]; then printf '%s\\n' \"$@\" > " + textutil.ShellQuote(argvFile) + "; fi\n"
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
	t.Cleanup(StubMacAppCLI("")) // a Mac with Tailscale.app would otherwise answer
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd(nil) })
	if code == 0 {
		t.Errorf("status with no CLI on PATH: code = 0, want nonzero; stderr=%q", stderr)
	}
}

func TestTailscaleStatusRunningTextAndJSON(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, _ := hosttest.CaptureOutput(t, func() int { return Cmd(nil) })
	if code != 0 {
		t.Fatalf("status exit %d", code)
	}
	if !strings.Contains(out, "test-mac.tailxxxxx.ts.net.") {
		t.Errorf("status text output missing node name:\n%s", out)
	}

	code, out, _ = hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--format", "json"}) })
	if code != 0 || !strings.Contains(out, "test-mac.tailxxxxx.ts.net.") {
		t.Errorf("status --format json: code=%d out=%q", code, out)
	}
}

func TestTailscaleConnectWithoutKeyAlreadyRunningExitsZero(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, _ := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--connect"}) })
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
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--connect"}) })
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
	if err := vault.Store("TAILSCALE_AUTHKEY", "tskey-auth-fake"); err != nil {
		t.Fatal(err)
	}
	_, out, stderr := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--connect"}) })
	if strings.Contains(out+stderr, "no auth key given") {
		t.Errorf("connect did not use the vault auth key: stdout=%q stderr=%q", out, stderr)
	}
}

// TestTailscaleConnectAuthKeyDashReadsStdin: `--auth-key -` reads stdin and the key never reaches `up`'s argv.
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

	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--connect",
			"--auth-key", "-"})
	})
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

// TestTailscaleConnectLiteralAuthKeyWarns: a literal --auth-key connects with a warning, never on `up`'s argv.
func TestTailscaleConnectLiteralAuthKeyWarns(t *testing.T) {
	hermeticTailscaleSecrets(t)
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	fakeTailscaleUpOnPATH(t, argvFile)

	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--connect", "--auth-key", "tskey-auth-literal"})
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
			ts := tailscaleAuthKeyServer(t, key)
			defer ts.Close()
			routeDefaultClientTo(t, ts)
			args := []string{"--key"}
			if save {
				args = append(args, "--save")
			}
			code, out, stderr := hosttest.CaptureOutput(t, func() int { return Cmd(args) })
			if code != 0 {
				t.Fatalf("key exit %d, want 0; stderr=%q", code, stderr)
			}
			if out != key+"\n" {
				t.Errorf("stdout = %q, want only %q", out, key+"\n")
			}
			checkTailscaleSavedKey(t, save, key, stderr)
		})
	}
}

// Reuse the CreateAuthKey response pattern from internal/network/tailscale/api_test.go.
func tailscaleAuthKeyServer(t *testing.T, key string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/tailnet/-/keys" {
			t.Errorf("unexpected admin API request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(AuthKey{ID: "k1", Key: key}); err != nil {
			t.Error(err)
		}
	}))
}

func checkTailscaleSavedKey(t *testing.T, save bool, key, stderr string) {
	t.Helper()
	stored, source, ok := vault.Get("TAILSCALE_AUTHKEY")
	if save {
		if !ok || stored != key || source != "vault" {
			t.Errorf("saved key = %q, source=%q, found=%v", stored, source, ok)
		}
		want := "saved TAILSCALE_AUTHKEY to the vault (" + vault.Redact(key) + ")\n"
		if stderr != want {
			t.Errorf("stderr = %q, want %q", stderr, want)
		}
		return
	}
	if ok {
		t.Error("key without --save must not store TAILSCALE_AUTHKEY")
	}
	if stderr != "" {
		t.Errorf("key without --save stderr = %q, want empty", stderr)
	}
}

// TestTailscaleKeyTrimsTags: --tags "tag:a, tag:b" must reach the admin API as ["tag:a","tag:b"], not " tag:b".
func TestTailscaleKeyTrimsTags(t *testing.T) {
	hermeticTailscaleSecrets(t)
	t.Setenv("TAILSCALE_API_KEY", "test-api-key")
	var got []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Capabilities struct {
				Devices struct{ Create struct{ Tags []string } }
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		got = body.Capabilities.Devices.Create.Tags
		_ = json.NewEncoder(w).Encode(AuthKey{ID: "k1", Key: "tskey-auth-xyz"})
	}))
	defer ts.Close()
	routeDefaultClientTo(t, ts)
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--key", "--tags", "tag:a, tag:b,"})
	})
	if code != 0 {
		t.Fatalf("key exit %d, want 0; stderr=%q", code, stderr)
	}
	if want := []string{"tag:a", "tag:b"}; !slices.Equal(got, want) {
		t.Errorf("tags sent = %q, want %q", got, want)
	}
}

func TestTailscaleKeyWithoutCredentialFails(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--key"}) })
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
	code, out, _ := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--exit-node",
			"approve"})
	})
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
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd(tc.args) })
			if code != 2 {
				t.Errorf("exit %d, want 2; stderr=%q", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q lacks %q", stderr, tc.want)
			}
		})
	}
}

func TestNetworkTailscaleRouteEmptyValueExitsTwo(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--route", ""}) })
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
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--route",
			"-invalid"})
	})
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
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd(tc.args) })
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
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
				return Cmd([]string{"--route",
					args})
			})
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
			code, _, stderr := hosttest.CaptureOutput(t, func() int { return Cmd(tc.args) })
			if code != 2 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d stderr %q, want 2 and %q", code, stderr, tc.want)
			}
		})
	}
}

func TestNetworkTailscaleSubnetApproveWithoutCredentialPrintsAdminURL(t *testing.T) {
	hermeticTailscaleSecrets(t)
	fakeTailscaleOnPATH(t, "Running")
	code, out, _ := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--subnet", "approve"}) })
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
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
				return Cmd([]string{"--subnet",
					args})
			})
			if code != 0 || !strings.Contains(stdout, want) {
				t.Errorf("--subnet %s: exit %d stdout %q stderr %q, want 0 and %q", args, code, stdout, stderr, want)
			}
		})
	}
}

func fakeAdvertiseCLI(t *testing.T, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nif [ \"$1\" = status ]; then\n" +
		"printf '%s\\n' '{\"BackendState\":\"Running\",\"Self\":{\"DNSName\":\"test-node.ts.net.\"}}'\nexit 0\nfi\n" +
		"printf '%s\\n' \"$@\" > " + textutil.ShellQuote(argv) + "\n" + fmt.Sprintf("exit %d\n", exitCode)
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return argv
}

func TestTailscaleKeylessConnectAppliesAdvertise(t *testing.T) {
	for _, exitCode := range []int{0, 1} {
		t.Run(fmt.Sprintf("set_exit_%d", exitCode), func(t *testing.T) {
			hermeticTailscaleSecrets(t)
			argv := fakeAdvertiseCLI(t, exitCode)
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
				return Cmd([]string{"--connect", "--advertise-exit"})
			})
			if code != exitCode {
				t.Fatalf("advertise exit=%d, want %d; stdout=%q stderr=%q", code, exitCode, stdout, stderr)
			}
			got, err := os.ReadFile(argv)
			if err != nil {
				t.Fatalf("keyless connect never applied advertising: %v", err)
			}
			if string(got) != "set\n--advertise-exit-node\n" {
				t.Fatalf("advertise argv=%q", got)
			}
		})
	}
}

func captureClosedStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		if err := os.Stdout.Close(); err != nil {
			t.Fatal(err)
		}
		return fn()
	})
	return code, stderr
}

func TestTailscaleKeyReportsStdoutFailure(t *testing.T) {
	hermeticTailscaleSecrets(t)
	t.Setenv("TAILSCALE_API_KEY", "test-api-key")
	srv := tailscaleAuthKeyServer(t, "tskey-auth-test-output")
	defer srv.Close()
	routeDefaultClientTo(t, srv)
	code, stderr := captureClosedStdout(t, func() int { return Cmd([]string{"--key"}) })
	if code != 1 || !strings.Contains(stderr, "writing key to stdout") {
		t.Fatalf("undelivered auth key: exit=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stderr, "tskey-auth-test-output") {
		t.Fatalf("auth key leaked into stderr: %q", stderr)
	}
}
