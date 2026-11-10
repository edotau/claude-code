package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

func TestClaudeScienceAssetName(t *testing.T) {
	setupToolsTestEnv(t)
	for _, tc := range []struct{ goos, goarch, want string }{
		{"linux", "amd64", "linux-x64"},
		{"darwin", "arm64", "mac-arm64.dmg"},
		{"darwin", "amd64", "mac-x64.dmg"},
		{"windows", "amd64", ""},
	} {
		if got := claudeScienceAssetName(tc.goos, tc.goarch); got != tc.want {
			t.Errorf("claudeScienceAssetName(%s,%s) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

func TestRunInstallScriptDryRunTouchesNoNetwork(t *testing.T) {
	setupToolsTestEnv(t)
	var buf bytes.Buffer
	// An unreachable URL proves dry-run never attempts the download: a real GET would error, not print success.
	if err := runInstallScript(context.Background(), &buf, true, "http://127.0.0.1:1/nope.sh", nil, nil); err != nil {
		t.Fatalf("dry-run must not touch the network: %v", err)
	}
	if !strings.Contains(buf.String(), "dry-run") {
		t.Errorf("output = %q, want a dry-run marker", buf.String())
	}
}

func TestInstallClaudeScienceDryRunTouchesNoNetwork(t *testing.T) {
	setupToolsTestEnv(t)
	old := claudeScienceBaseURL
	claudeScienceBaseURL = "http://127.0.0.1:1/claude-science"
	defer func() { claudeScienceBaseURL = old }()
	var buf bytes.Buffer
	if err := installClaudeScience(context.Background(), &buf, localBinDir(), true); err != nil {
		t.Fatalf("dry-run must not touch the network: %v", err)
	}
	if !strings.Contains(buf.String(), "dry-run") {
		t.Errorf("output = %q, want a dry-run marker", buf.String())
	}
}

func TestInstallCopilotFallsBackToBinDirForNonBinDir(t *testing.T) {
	setupToolsTestEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#!/bin/sh\nprintf 'PREFIX=%s\\n' \"$PREFIX\"\n"))
	}))
	defer srv.Close()
	old := copilotInstallScriptURL
	copilotInstallScriptURL = srv.URL
	defer func() { copilotInstallScriptURL = old }()

	notBin := filepath.Join(t.TempDir(), "somewhere-else")
	var out bytes.Buffer
	if err := installCopilot(context.Background(), &out, notBin, false); err != nil {
		t.Fatalf("installCopilot: %v", err)
	}
	wantPrefix := filepath.Dir(paths.BinDir())
	if !strings.Contains(out.String(), "PREFIX="+wantPrefix) {
		t.Errorf("output = %q, want PREFIX=%s (BinDir fallback, not filepath.Dir(%q))", out.String(), wantPrefix, notBin)
	}
	if !strings.Contains(out.String(), "not a bin/ dir; installing into BinDir instead") {
		t.Errorf("output = %q, want the fallback notice", out.String())
	}
}

func TestInstallCopilotUsesDirDirectlyWhenNamedBin(t *testing.T) {
	setupToolsTestEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#!/bin/sh\nprintf 'PREFIX=%s\\n' \"$PREFIX\"\n"))
	}))
	defer srv.Close()
	old := copilotInstallScriptURL
	copilotInstallScriptURL = srv.URL
	defer func() { copilotInstallScriptURL = old }()

	dir := filepath.Join(t.TempDir(), "bin")
	var out bytes.Buffer
	if err := installCopilot(context.Background(), &out, dir, false); err != nil {
		t.Fatalf("installCopilot: %v", err)
	}
	wantPrefix := filepath.Dir(dir)
	if !strings.Contains(out.String(), "PREFIX="+wantPrefix) {
		t.Errorf("output = %q, want PREFIX=%s (dir's own parent)", out.String(), wantPrefix)
	}
	if strings.Contains(out.String(), "installing into BinDir instead") {
		t.Errorf("output = %q, must not fall back when dir is already named bin/", out.String())
	}
}

func TestInstallOpencodeCopiesFromHomeInstallDir(t *testing.T) {
	setupToolsTestEnv(t)
	const installed = "#!/bin/sh\nprintf 'opencode 1.0.0\\n'\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`#!/bin/sh
mkdir -p "$HOME/.opencode/bin"
cat > "$HOME/.opencode/bin/opencode" <<'EOF'
#!/bin/sh
printf 'opencode 1.0.0\n'
EOF
chmod 755 "$HOME/.opencode/bin/opencode"
`))
	}))
	defer srv.Close()
	oldURL := opencodeInstallScriptURL
	opencodeInstallScriptURL = srv.URL
	defer func() { opencodeInstallScriptURL = oldURL }()

	dir := t.TempDir()
	var out bytes.Buffer
	if err := installOpencode(context.Background(), &out, dir, false); err != nil {
		t.Fatalf("installOpencode: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "opencode"))
	if err != nil {
		t.Fatalf("read copied binary: %v", err)
	}
	if string(got) != installed {
		t.Errorf("copied contents = %q, want %q", got, installed)
	}
	info, err := os.Stat(filepath.Join(dir, "opencode"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("copied mode = %o, want 755", info.Mode().Perm())
	}
}

// opencodeInstallScript returns a fake install script whose installed binary reports version via --version.
func opencodeInstallScript(version string) string {
	return "#!/bin/sh\n" +
		`mkdir -p "$HOME/.opencode/bin"` + "\n" +
		`cat > "$HOME/.opencode/bin/opencode" <<'EOF'` + "\n" +
		"#!/bin/sh\n" +
		`if [ "$1" = "--version" ]; then printf '` + version + `\n'; else printf 'fake opencode\n'; fi` + "\n" +
		"EOF\n" +
		`chmod 755 "$HOME/.opencode/bin/opencode"` + "\n"
}

func installOpencodeFromFakeServer(t *testing.T, version, dir string) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(opencodeInstallScript(version)))
	}))
	defer srv.Close()
	oldURL := opencodeInstallScriptURL
	opencodeInstallScriptURL = srv.URL
	defer func() { opencodeInstallScriptURL = oldURL }()
	return installOpencode(context.Background(), &bytes.Buffer{}, dir, false)
}

// TestInstallOpencodeRefusesMajorV2 pins the guard against opencode 2.x: never a real network install.
func TestInstallOpencodeRefusesMajorV2(t *testing.T) {
	setupToolsTestEnv(t)
	dir := t.TempDir()
	err := installOpencodeFromFakeServer(t, "2.0.0", dir)
	if err == nil {
		t.Fatal("installOpencode: want refusal for opencode 2.0.0, got nil error")
	}
	wantSubstr := fmt.Sprintf("harness supports <%d.0", opencodeMaxMajor+1)
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("error = %q, want it to contain %q", err, wantSubstr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "opencode")); !os.IsNotExist(statErr) {
		t.Errorf("opencode 2.0.0 must not be linked into %s", dir)
	}
}

// TestInstallOpencodeRefusalRestoresPriorBinary confirms a refusal restores the pre-existing
// ~/.opencode/bin/opencode byte-for-byte, rather than leaving the rejected 2.x in place.
func TestInstallOpencodeRefusalRestoresPriorBinary(t *testing.T) {
	env := setupToolsTestEnv(t)
	priorContent := "#!/bin/sh\nprintf 'prior opencode 1.x\\n'\n"
	priorPath := filepath.Join(env.home, ".opencode", "bin", "opencode")
	writeToolsFixture(t, priorPath, priorContent, 0o755)

	dir := t.TempDir()
	if err := installOpencodeFromFakeServer(t, "2.0.0", dir); err == nil {
		t.Fatal("installOpencode: want refusal for opencode 2.0.0, got nil error")
	}
	got, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatalf("prior opencode binary must be restored: %v", err)
	}
	if string(got) != priorContent {
		t.Errorf("restored contents = %q, want byte-identical prior %q", got, priorContent)
	}
	if _, statErr := os.Stat(priorPath + ".prev"); !os.IsNotExist(statErr) {
		t.Errorf("aside copy %s.prev must not remain after restore", priorPath)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "opencode")); !os.IsNotExist(statErr) {
		t.Errorf("refused opencode 2.0.0 must not be linked into %s", dir)
	}
}

// TestInstallOpencodeRefusalWithNoPriorLeavesNone confirms a refusal with no pre-existing binary
// leaves none at ~/.opencode/bin/opencode (no stray 2.x, no stray aside copy).
func TestInstallOpencodeRefusalWithNoPriorLeavesNone(t *testing.T) {
	env := setupToolsTestEnv(t)
	priorPath := filepath.Join(env.home, ".opencode", "bin", "opencode")
	dir := t.TempDir()
	if err := installOpencodeFromFakeServer(t, "2.0.0", dir); err == nil {
		t.Fatal("installOpencode: want refusal for opencode 2.0.0, got nil error")
	}
	if _, statErr := os.Stat(priorPath); !os.IsNotExist(statErr) {
		t.Errorf("refusal with no prior binary must leave none at %s", priorPath)
	}
	if _, statErr := os.Stat(priorPath + ".prev"); !os.IsNotExist(statErr) {
		t.Errorf("aside copy %s.prev must not remain", priorPath)
	}
}

// TestInstallOpencodeSuccessLeavesNoAsideCopy confirms a successful install cleans up the
// aside copy it made of the prior binary, rather than leaving it behind.
func TestInstallOpencodeSuccessLeavesNoAsideCopy(t *testing.T) {
	env := setupToolsTestEnv(t)
	priorPath := filepath.Join(env.home, ".opencode", "bin", "opencode")
	writeToolsFixture(t, priorPath, "#!/bin/sh\nprintf 'prior opencode 1.x\\n'\n", 0o755)

	dir := t.TempDir()
	if err := installOpencodeFromFakeServer(t, "1.9.0", dir); err != nil {
		t.Fatalf("installOpencode: %v", err)
	}
	if _, statErr := os.Stat(priorPath + ".prev"); !os.IsNotExist(statErr) {
		t.Errorf("successful install must not leave an aside copy at %s.prev", priorPath)
	}
	if _, err := os.Stat(filepath.Join(dir, "opencode")); err != nil {
		t.Errorf("opencode 1.9.0 must be linked into %s: %v", dir, err)
	}
}

// TestInstallOpencodeAllowsMajorV1 confirms the guard does not block versions at or below the pin.
func TestInstallOpencodeAllowsMajorV1(t *testing.T) {
	setupToolsTestEnv(t)
	dir := t.TempDir()
	if err := installOpencodeFromFakeServer(t, "1.9.0", dir); err != nil {
		t.Fatalf("installOpencode: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "opencode")); err != nil {
		t.Errorf("opencode 1.9.0 must be linked into %s: %v", dir, err)
	}
}

// TestRestoreOpencodeAsideNoAsideReturnsCauseUnchanged confirms that with no aside copy to restore,
// the cause error passes through unchanged (not wrapped or joined).
func TestRestoreOpencodeAsideNoAsideReturnsCauseUnchanged(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "opencode")
	aside := src + ".prev" // never created
	cause := errors.New("install refused")
	got := restoreOpencodeAside(src, aside, cause)
	if got != cause {
		t.Errorf("restoreOpencodeAside with no aside = %v, want the cause error unchanged (%v)", got, cause)
	}
}

// TestRestoreOpencodeAsideRenameFailureJoinsCause confirms a failed restore-rename is joined with
// cause, not swallowed, so a lost prior binary is never just a dropped warning.
func TestRestoreOpencodeAsideRenameFailureJoinsCause(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits don't block rename the same way on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "opencode")
	aside := src + ".prev"
	if err := os.WriteFile(aside, []byte("prior"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // let t.TempDir() clean up afterward
	cause := errors.New("install refused")
	got := restoreOpencodeAside(src, aside, cause)
	if got == nil {
		t.Fatal("restoreOpencodeAside: want a joined error when the restore rename fails, got nil")
	}
	if !errors.Is(got, cause) {
		t.Errorf("error = %v, want it to join (and preserve) the cause", got)
	}
	if !strings.Contains(got.Error(), "restore previous binary") {
		t.Errorf("error = %q, want it to also name the restore-rename failure", got)
	}
}

// TestInstallOpencodeAllowMajorEnvOverride confirms the escape hatch lifts the refusal for one install.
func TestInstallOpencodeAllowMajorEnvOverride(t *testing.T) {
	setupToolsTestEnv(t)
	t.Setenv(opencodeAllowMajorEnv, "1")
	dir := t.TempDir()
	if err := installOpencodeFromFakeServer(t, "2.0.0", dir); err != nil {
		t.Fatalf("installOpencode with override: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "opencode")); err != nil {
		t.Errorf("override must still link opencode into %s: %v", dir, err)
	}
}

// TestInstallOpencodeRefusesUnknownVersion: an unparsable probe (as a timeout yields) is refused and the prior binary kept.
func TestInstallOpencodeRefusesUnknownVersion(t *testing.T) {
	env := setupToolsTestEnv(t)
	priorContent := "#!/bin/sh\nprintf 'prior opencode 1.x\\n'\n"
	priorPath := filepath.Join(env.home, ".opencode", "bin", "opencode")
	writeToolsFixture(t, priorPath, priorContent, 0o755)
	dir := t.TempDir()
	if err := installOpencodeFromFakeServer(t, "", dir); err == nil {
		t.Fatal("installOpencode: want refusal for an unknown version, got nil error")
	}
	if got, _ := os.ReadFile(priorPath); string(got) != priorContent {
		t.Errorf("prior binary = %q, want restored %q", got, priorContent)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "opencode")); !os.IsNotExist(statErr) {
		t.Errorf("unknown-version opencode must not be linked into %s", dir)
	}
}

// stubScienceTools fakes hdiutil/codesign/ditto: attach builds an app at the mountpoint, signed by team.
func stubScienceTools(t *testing.T, team string) {
	t.Helper()
	old := scienceRun
	t.Cleanup(func() { scienceRun = old })
	scienceRun = func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch {
		case name == "hdiutil" && args[0] == "attach":
			cli := filepath.Join(args[len(args)-2], scienceAppName, "Contents/Resources/bin/claude-science")
			if err := os.MkdirAll(filepath.Dir(cli), 0o755); err != nil {
				return nil, err
			}
			return nil, os.WriteFile(cli, []byte("#!/bin/sh\necho claude-science 9.9\n"), 0o755)
		case name == "codesign" && args[0] == "-dv":
			return []byte("Identifier=x\nTeamIdentifier=" + team + "\n"), nil
		case name == "ditto":
			return exec.Command("cp", "-R", args[0], args[1]).CombinedOutput()
		}
		return nil, nil
	}
}

func runScienceInstall(t *testing.T, team string) (root, bin string, err error) {
	setupToolsTestEnv(t)
	if runtime.GOOS != "darwin" {
		t.Skip("macOS app install")
	}
	stubScienceTools(t, team)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("dmg")) }))
	t.Cleanup(srv.Close)
	oldURL, oldRoots := claudeScienceBaseURL, scienceAppRoots
	root, bin = filepath.Join(t.TempDir(), "Applications"), t.TempDir()
	claudeScienceBaseURL, scienceAppRoots = srv.URL, func() []string { return []string{root} }
	t.Cleanup(func() { claudeScienceBaseURL, scienceAppRoots = oldURL, oldRoots })
	return root, bin, installClaudeScience(context.Background(), &bytes.Buffer{}, bin, false)
}

func TestInstallClaudeScienceDarwinInstallsAppAndLinksCLI(t *testing.T) {
	root, bin, err := runScienceInstall(t, scienceTeamID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join(bin, "claude-science"))
	if want := filepath.Join(root, scienceAppName, "Contents/Resources/bin/claude-science"); err != nil || got != want {
		t.Fatalf("link = %q, %v; want %q", got, err, want)
	}
	if out, err := exec.Command(filepath.Join(bin, "claude-science")).CombinedOutput(); err != nil || !strings.Contains(string(out), "9.9") {
		t.Errorf("run via link = %q, %v", out, err)
	}
}

func TestInstallClaudeScienceDarwinRefusesForeignSigner(t *testing.T) {
	root, bin, err := runScienceInstall(t, "OTHERTEAM")
	if err == nil || !strings.Contains(err.Error(), "not signed by team") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	for _, dir := range []string{root, bin} {
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s holds %d entries after a refused install", dir, len(entries))
		}
	}
}

// agy installs where Link looks (~/.local/bin), not into the bin dir, or bin/gemini is never linked.
func TestInstallAgyDryRunPassesDir(t *testing.T) {
	setupToolsTestEnv(t)
	var buf bytes.Buffer
	if err := installAgy(context.Background(), &buf, t.TempDir(), true); err != nil {
		t.Fatal(err)
	}
	if want := agyInstallScriptURL + " [--dir " + localBinDir() + "]"; !strings.Contains(buf.String(), want) {
		t.Errorf("output = %q, want %q", buf.String(), want)
	}
}
