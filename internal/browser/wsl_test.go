package browser

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type probeRoundTripper func(*http.Request) (*http.Response, error)

func (f probeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type probeBody struct {
	io.Reader
	closed bool
}

func (b *probeBody) Close() error {
	b.closed = true
	return nil
}

func newWSLTestEnv(t *testing.T, f *fakeRun) Env {
	t.Helper()
	home := t.TempDir()
	return Env{
		WSL:        true,
		GOOS:       "linux",
		WinRoot:    t.TempDir(),
		Home:       home,
		AppSupport: filepath.Join(home, ".config"),
		Run:        f.run,
		Probe:      func() string { return "" },
		Sleep:      func(time.Duration) {},
		ClaudeBin:  "claude", HasNPX: true,
	}
}

func TestProbeCDPUsesDeadlineAndClosesBody(t *testing.T) {
	originalTransport := http.DefaultTransport
	defer func() { http.DefaultTransport = originalTransport }()

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "success", body: `{"Browser":"Chrome/120"}`, want: "Chrome/120"},
		{name: "decode error", body: `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responseBody := &probeBody{Reader: strings.NewReader(tc.body)}
			http.DefaultTransport = probeRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != "http://localhost:9222/json/version" {
					t.Errorf("request URL = %q, want CDP version endpoint", req.URL)
				}
				if _, ok := req.Context().Deadline(); !ok {
					t.Error("request context has no deadline")
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       responseBody,
					Request:    req,
				}, nil
			})

			if got := probeCDP(); got != tc.want {
				t.Errorf("probeCDP() = %q, want %q", got, tc.want)
			}
			if !responseBody.closed {
				t.Error("response body was not closed")
			}
		})
	}
}

func writeWSLExe(t *testing.T, env Env, name string) {
	t.Helper()
	paths := winExe[name]
	if len(paths) == 0 {
		t.Fatalf("no Windows executable registered for %q", name)
	}
	path := filepath.Join(env.WinRoot, paths[0])
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWSLDetect(t *testing.T) {
	tests := []struct {
		name    string
		browser string
		owner   string
		wantCDP string
	}{
		{name: "edge_available", browser: "edge", wantCDP: stateAvailable},
		{name: "edge_ready", browser: "edge", owner: "Edg/120.0 Chrome/119", wantCDP: stateReady},
		{name: "chrome_wrong_owner", browser: "chrome", owner: "Edg/120", wantCDP: stateAvailable},
		{name: "firefox_unsupported", browser: "firefox", owner: "Edg/120 Chrome/119", wantCDP: stateUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeRun{}
			env := newWSLTestEnv(t, f)
			writeWSLExe(t, env, tt.browser)
			probes := 0
			env.Probe = func() string {
				probes++
				return tt.owner
			}

			b := detectOne(env, specMust(t, tt.browser))
			if !b.Installed {
				t.Error("want Installed=true")
			}
			if b.Host != stateUnsupportedWSL {
				t.Errorf("Host = %q, want %q", b.Host, stateUnsupportedWSL)
			}
			if b.Paths.Extension != stateUnsupportedWSL {
				t.Errorf("Paths.Extension = %q, want %q", b.Paths.Extension, stateUnsupportedWSL)
			}
			if b.Paths.CDP != tt.wantCDP {
				t.Errorf("Paths.CDP = %q, want %q", b.Paths.CDP, tt.wantCDP)
			}
			if tt.browser == "firefox" && probes != 0 {
				t.Errorf("Probe calls = %d, want 0 for Firefox", probes)
			}
			if len(f.calls) != 0 {
				t.Errorf("Run calls = %q, want none for a Windows fixture", f.calls)
			}
		})
	}
}

func TestWSLDetectMissingWindowsExe(t *testing.T) {
	for _, s := range knownBrowsers {
		t.Run(s.name, func(t *testing.T) {
			f := &fakeRun{}
			env := newWSLTestEnv(t, f)
			env.Probe = func() string {
				t.Error("Probe must not run without a Windows executable")
				return ""
			}

			b := detectOne(env, s)
			if b.Installed {
				t.Error("want Installed=false")
			}
			if b.Host != stateNotInstalled {
				t.Errorf("Host = %q, want %q", b.Host, stateNotInstalled)
			}
			if s.linuxBinary != "" {
				want := []string{"sh -c command -v " + s.linuxBinary}
				if !reflect.DeepEqual(f.calls, want) {
					t.Errorf("Run calls = %q, want Linux fallback %q", f.calls, want)
				}
			} else if len(f.calls) != 0 {
				t.Errorf("Run calls = %q, want none without a Linux binary", f.calls)
			}
		})
	}
}

func TestWSLDefault(t *testing.T) {
	const userChoice = `powershell.exe -NoProfile -Command (Get-ItemProperty 'HKCU:\Software\Microsoft\Windows\Shell\Associations\UrlAssociations\https\UserChoice').ProgId`
	tests := []struct {
		name    string
		progID  string
		browser string
		wantErr error
	}{
		{name: "edge_prefix", progID: "MSEdgeHTM.something", browser: "edge"},
		{name: "chrome", progID: "ChromeHTML", browser: "chrome"},
		{name: "unknown", progID: "UnknownBrowserHTML", wantErr: ErrDefaultUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeRun{answers: map[string]fakeAnswer{
				userChoice: {out: tt.progID},
			}}
			env := newWSLTestEnv(t, f)
			if tt.browser != "" {
				writeWSLExe(t, env, tt.browser)
			}

			b, err := Default(env)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Default error = %v, want %v", err, tt.wantErr)
			}
			if b.Name != tt.browser {
				t.Errorf("Browser.Name = %q, want %q", b.Name, tt.browser)
			}
			if tt.browser != "" && (!b.Installed || !b.Default || b.Host != stateUnsupportedWSL) {
				t.Errorf("Browser = %+v, want installed WSL default", b)
			}
			if !reflect.DeepEqual(f.calls, []string{userChoice}) {
				t.Errorf("Run calls = %q, want only %q", f.calls, userChoice)
			}
		})
	}
}

func TestWSLSetupCDP(t *testing.T) {
	tests := []struct {
		name         string
		alreadyReady bool
		readyOnStart bool
		dryRun       bool
		wantKind     string
		wantExecuted bool
		wantError    string
		wantProbes   int
		wantSleeps   int
	}{
		{name: "already_running", alreadyReady: true, wantKind: "noop", wantExecuted: true, wantProbes: 1},
		{name: "starts_ready", readyOnStart: true, wantKind: "start-cdp", wantExecuted: true, wantProbes: 2},
		{name: "poll_exhausted", wantKind: "start-cdp", wantError: "networkingMode=mirrored", wantProbes: 21, wantSleeps: 20},
		{name: "dry_run", dryRun: true, wantKind: "start-cdp", wantProbes: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeRun{}
			env := newWSLTestEnv(t, f)
			writeWSLExe(t, env, "edge")
			env.DryRun = tt.dryRun
			probes, sleeps := 0, 0
			env.Probe = func() string {
				probes++
				if tt.alreadyReady || (tt.readyOnStart && len(f.calls) > 0) {
					return "Edg/120"
				}
				return ""
			}
			env.Sleep = func(d time.Duration) {
				sleeps++
				if d != 500*time.Millisecond {
					t.Errorf("Sleep duration = %v, want 500ms", d)
				}
			}

			acts := setupWSL(env, Browser{Name: "edge", DisplayName: "Microsoft Edge"})
			if acts.Browser != "edge" || len(acts.Items) != 3 {
				t.Fatalf("Actions = %+v, want edge with 3 items", acts)
			}
			a := acts.Items[1]
			if a.Kind != tt.wantKind || a.Executed != tt.wantExecuted {
				t.Errorf("CDP action = %+v, want Kind=%q Executed=%v", a, tt.wantKind, tt.wantExecuted)
			}
			if tt.wantError == "" && a.Error != "" {
				t.Errorf("Error = %q, want empty", a.Error)
			} else if tt.wantError != "" && !strings.Contains(a.Error, tt.wantError) {
				t.Errorf("Error = %q, want substring %q", a.Error, tt.wantError)
			}
			if probes != tt.wantProbes || sleeps != tt.wantSleeps {
				t.Errorf("Probe/Sleep calls = %d/%d, want %d/%d", probes, sleeps, tt.wantProbes, tt.wantSleeps)
			}
			var wantCalls []string
			if tt.wantKind == "start-cdp" {
				if len(a.Command) != 4 || !reflect.DeepEqual(a.Command[:3], []string{"powershell.exe", "-NoProfile", "-Command"}) {
					t.Fatalf("Command = %q, want PowerShell script argv", a.Command)
				}
				ps := a.Command[len(a.Command)-1]
				for _, fragment := range []string{"--remote-debugging-port=9222", "edge-cdp"} {
					if !strings.Contains(ps, fragment) {
						t.Errorf("PowerShell script = %q, want substring %q", ps, fragment)
					}
				}
				if !tt.dryRun {
					wantCalls = append(wantCalls, strings.Join(a.Command, " "))
				}
				if tt.wantError != "" { // the failure path queries stale profile processes
					wantCalls = append(wantCalls, f.calls[1])
				}
			}
			if !tt.dryRun {
				wantCalls = append(wantCalls, strings.Join(acts.Items[2].Command, " "))
			}
			if !reflect.DeepEqual(f.calls, wantCalls) {
				t.Errorf("Run calls = %q, want %q", f.calls, wantCalls)
			}
		})
	}
}

func TestWSLSetupRegisterMCP(t *testing.T) {
	tests := []struct {
		name       string
		registered bool
		wantKind   string
	}{
		{name: "missing_config", wantKind: "register-mcp"},
		{name: "already_registered", registered: true, wantKind: "noop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeRun{}
			env := newWSLTestEnv(t, f)
			writeWSLExe(t, env, "edge")
			env.Probe = func() string { return "Edg/120" }
			if tt.registered {
				if err := os.WriteFile(filepath.Join(env.Home, ".claude.json"),
					[]byte(`{"mcpServers":{"chrome-devtools":{}}}`), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			acts := setupWSL(env, Browser{Name: "edge", DisplayName: "Microsoft Edge"})
			if len(acts.Items) != 3 {
				t.Fatalf("Items = %+v, want 3 actions", acts.Items)
			}
			a := acts.Items[2]
			if a.Kind != tt.wantKind || !a.Executed || a.Error != "" {
				t.Errorf("MCP action = %+v, want executed %q with no error", a, tt.wantKind)
			}
			wantCommand := []string{"claude", "mcp", "add", "-s", "user", "chrome-devtools", "--",
				"npx", "-y", "chrome-devtools-mcp@1.11.0", "--browserUrl", "http://localhost:9222"}
			if tt.registered {
				if len(f.calls) != 0 || len(a.Command) != 0 {
					t.Errorf("Run calls = %q, Command = %q, want no registration", f.calls, a.Command)
				}
			} else {
				if !reflect.DeepEqual(a.Command, wantCommand) {
					t.Errorf("Command = %q, want %q", a.Command, wantCommand)
				}
				if !reflect.DeepEqual(f.calls, []string{strings.Join(wantCommand, " ")}) {
					t.Errorf("Run calls = %q, want only MCP registration", f.calls)
				}
			}
		})
	}
}

func TestCDPFailureDiagnosis(t *testing.T) {
	b := Browser{Name: "edge", DisplayName: "Microsoft Edge"}
	mk := func(t *testing.T, pids, wslconfig string) Env {
		env := newWSLTestEnv(t, &fakeRun{})
		env.Run = func(string, ...string) (string, error) { return pids, nil }
		if wslconfig != "" {
			dir := filepath.Join(env.WinRoot, "Users", "u")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".wslconfig"), []byte(wslconfig), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return env
	}
	got := cdpFailure(mk(t, "111\n222\n", "[wsl2]\nnetworkingMode=mirrored\n"), b)
	if !strings.Contains(got, "111, 222") || !strings.Contains(got, "Stop-Process -Id 111,222") {
		t.Errorf("stale pids not named: %s", got)
	}
	got = cdpFailure(mk(t, "", "[wsl2]\nnetworkingMode=mirrored\n"), b)
	if strings.Contains(got, "mirrored") {
		t.Errorf("mirrored set but blamed: %s", got)
	}
	got = cdpFailure(mk(t, "", ""), b)
	if !strings.Contains(got, "networkingMode=mirrored") {
		t.Errorf("missing mirrored not named: %s", got)
	}
}

func TestMirroredIn(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       bool
	}{
		{"set", "[wsl2]\nnetworkingMode=mirrored", true},
		{"case and spaces", "[WSL2]\n NetworkingMode = Mirrored # on\n", true},
		{"other section", "[experimental]\nnetworkingMode=mirrored\n", false},
		{"commented", "[wsl2]\n#networkingMode=mirrored\n", false},
		{"nat", "[wsl2]\nnetworkingMode=nat\n", false},
	} {
		if got := mirroredIn(tc.text); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}
