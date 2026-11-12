package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

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

func TestInstallCopilotFallsBackToBinDirForNonBinDir(t *testing.T) {
	skipWithoutPOSIXShell(t)
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
	skipWithoutPOSIXShell(t)
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
	skipWithoutPOSIXShell(t)
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

// skipWithoutPOSIXShell skips fake-installer tests: runInstallScript execs bash and the fakes are sh scripts.
func skipWithoutPOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("installer scripts run under bash/sh")
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
	skipWithoutPOSIXShell(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(opencodeInstallScript(version)))
	}))
	defer srv.Close()
	oldURL := opencodeInstallScriptURL
	opencodeInstallScriptURL = srv.URL
	defer func() { opencodeInstallScriptURL = oldURL }()
	return installOpencode(context.Background(), &bytes.Buffer{}, dir, false)
}

// TestInstallOpencodeRefusesMajorV2 pins the guard against opencode 2.x; with no prior binary it leaves none, no aside.
func TestInstallOpencodeRefusesMajorV2(t *testing.T) {
	env := setupToolsTestEnv(t)
	priorPath := filepath.Join(env.home, ".opencode", "bin", "opencode")
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
	for _, p := range []string{priorPath, priorPath + ".prev"} {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Errorf("refusal with no prior binary must leave nothing at %s", p)
		}
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

func TestCodexAssetName(t *testing.T) {
	setupToolsTestEnv(t)
	for _, tc := range []struct{ goos, goarch, want string }{
		{"linux", "amd64", "codex-x86_64-unknown-linux-musl.tar.gz"},
		{"linux", "arm64", "codex-aarch64-unknown-linux-musl.tar.gz"},
		{"darwin", "arm64", "codex-aarch64-apple-darwin.tar.gz"},
		{"windows", "amd64", ""},
		{"linux", "386", ""},
	} {
		if got := codexAssetName(tc.goos, tc.goarch); got != tc.want {
			t.Errorf("codexAssetName(%s,%s) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

func TestSelectCodexAsset(t *testing.T) {
	setupToolsTestEnv(t)
	rel := ghRelease{TagName: "rust-v1.2.3"}
	rel.Assets = append(rel.Assets,
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{Name: "codex-x86_64-unknown-linux-musl.tar.gz", URL: "https://example/linux.tar.gz"},
		struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{Name: "codex-package-x86_64-unknown-linux-musl.tar.gz", URL: "https://example/decoy.tar.gz"},
	)
	url, name, err := selectCodexAsset(rel, "linux", "amd64")
	if err != nil || url != "https://example/linux.tar.gz" || name != "codex-x86_64-unknown-linux-musl.tar.gz" {
		t.Fatalf("selectCodexAsset = %q %q %v", url, name, err)
	}
	if _, _, err := selectCodexAsset(rel, "darwin", "arm64"); err == nil {
		t.Error("missing asset must error, not silently pick the linux one")
	}
	if _, _, err := selectCodexAsset(rel, "plan9", "amd64"); err == nil {
		t.Error("unmapped platform must error")
	}
}

// buildTarGz packs name=content as the sole entry, the shape every codex release asset uses.
func buildTarGz(t *testing.T, name, content string) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return gzBuf.Bytes()
}

func TestExtractTarGzSingleFile(t *testing.T) {
	setupToolsTestEnv(t)
	data := buildTarGz(t, "codex-x86_64-unknown-linux-musl", "fake-elf-bytes")
	got, err := extractTarGzSingleFile(data)
	if err != nil || string(got) != "fake-elf-bytes" {
		t.Fatalf("extractTarGzSingleFile = %q %v", got, err)
	}
	if _, err := extractTarGzSingleFile([]byte("not gzip")); err == nil {
		t.Error("garbage input must error")
	}
}

func TestInstallCodexDryRunUsesAssetFromServer(t *testing.T) {
	setupToolsTestEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tag_name":"rust-v9.9.9","assets":[
			{"name":"codex-x86_64-unknown-linux-musl.tar.gz","browser_download_url":"http://unreachable.invalid/codex.tar.gz"}
		]}`))
	}))
	defer srv.Close()
	oldURL, oldGOOS, oldGOARCH := codexReleaseURL, goosOverride, goarchOverride
	codexReleaseURL = srv.URL
	goosOverride, goarchOverride = "linux", "amd64"
	defer func() { codexReleaseURL, goosOverride, goarchOverride = oldURL, oldGOOS, oldGOARCH }()

	var buf bytes.Buffer
	if err := installCodex(context.Background(), &buf, localBinDir(), true); err != nil {
		t.Fatalf("installCodex dry-run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "rust-v9.9.9") || !strings.Contains(out, "codex-x86_64-unknown-linux-musl.tar.gz") {
		t.Errorf("dry-run output missing resolved asset: %q", out)
	}
	if !strings.Contains(out, "dry-run") {
		t.Errorf("dry-run output missing the dry-run marker: %q", out)
	}
}

// TestInstallCodexDryRunNamesBinDirForNonBinDir pins the installCopilot pattern: a dir whose
// basename isn't "bin" must be corrected to BinDir, never installed into as given.
func TestInstallCodexDryRunNamesBinDirForNonBinDir(t *testing.T) {
	setupToolsTestEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tag_name":"rust-v9.9.9","assets":[
			{"name":"codex-x86_64-unknown-linux-musl.tar.gz","browser_download_url":"http://unreachable.invalid/codex.tar.gz"}
		]}`))
	}))
	defer srv.Close()
	oldURL, oldGOOS, oldGOARCH := codexReleaseURL, goosOverride, goarchOverride
	codexReleaseURL = srv.URL
	goosOverride, goarchOverride = "linux", "amd64"
	defer func() { codexReleaseURL, goosOverride, goarchOverride = oldURL, oldGOOS, oldGOARCH }()

	notBin := filepath.Join(t.TempDir(), "opt-homebrew-bin")
	var out bytes.Buffer
	if err := installCodex(context.Background(), &out, notBin, true); err != nil {
		t.Fatalf("installCodex dry-run: %v", err)
	}
	if !strings.Contains(out.String(), paths.BinDir()) {
		t.Errorf("dry-run output = %q, want it to name BinDir %s", out.String(), paths.BinDir())
	}
	if strings.Contains(out.String(), notBin) {
		t.Errorf("dry-run output = %q, must not reference the non-BinDir dir %s", out.String(), notBin)
	}
}

// TestInstallCodexInstallsBothBinariesIntoBinDir confirms codex and its companion land as real,
// executable regular files in BinDir, even when called with a non-BinDir dir (PATH's own dir).
func TestInstallCodexInstallsBothBinariesIntoBinDir(t *testing.T) {
	setupToolsTestEnv(t)
	codexTar := buildTarGz(t, "codex-x86_64-unknown-linux-musl", "fake-codex-elf")
	hostTar := buildTarGz(t, "codex-code-mode-host-x86_64-unknown-linux-musl", "fake-host-elf")
	assets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "host") {
			w.Write(hostTar)
			return
		}
		w.Write(codexTar)
	}))
	defer assets.Close()

	relSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"rust-v9.9.9","assets":[
			{"name":"codex-x86_64-unknown-linux-musl.tar.gz","browser_download_url":"%s/codex.tar.gz"},
			{"name":"codex-code-mode-host-x86_64-unknown-linux-musl.tar.gz","browser_download_url":"%s/host.tar.gz"}
		]}`, assets.URL, assets.URL)
	}))
	defer relSrv.Close()

	oldURL, oldGOOS, oldGOARCH := codexReleaseURL, goosOverride, goarchOverride
	codexReleaseURL = relSrv.URL
	goosOverride, goarchOverride = "linux", "amd64"
	defer func() { codexReleaseURL, goosOverride, goarchOverride = oldURL, oldGOOS, oldGOARCH }()

	notBin := filepath.Join(t.TempDir(), "opt-homebrew-bin")
	var out bytes.Buffer
	if err := installCodex(context.Background(), &out, notBin, false); err != nil {
		t.Fatalf("installCodex: %v", err)
	}
	for name, want := range map[string]string{"codex": "fake-codex-elf", "codex-code-mode-host": "fake-host-elf"} {
		assertInstalledCodexBinary(t, name, want)
	}
	if _, err := os.Stat(filepath.Join(notBin, "codex")); !os.IsNotExist(err) {
		t.Errorf("codex must not land in the non-BinDir dir %s", notBin)
	}
}

func assertInstalledCodexBinary(t *testing.T, name, want string) {
	t.Helper()
	p := filepath.Join(paths.BinDir(), name)
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	if string(got) != want {
		t.Errorf("%s contents = %q, want %q", p, got, want)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("%s is not a regular file (mode %v)", p, info.Mode())
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s not executable: mode %o", p, info.Mode().Perm())
	}
}

func TestCodexRollbackSerializesConcurrentRelease(t *testing.T) {
	setupToolsTestEnv(t)
	dir := paths.BinDir()
	host := "codex-code-mode-host"
	writeToolsFixture(t, filepath.Join(dir, host), "original host", 0o755)
	writeToolsFixture(t, filepath.Join(dir, "codex"), "original codex", 0o755)
	// The target fits NAME_MAX, but AtomicWrite's longer temporary basename forces the second write to fail.
	badName := "codex-" + strings.Repeat("x", 244)
	writeToolsFixture(t, filepath.Join(dir, badName), "original failed codex", 0o755)
	gate := &transactionGate{paused: make(chan struct{}), resume: make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(gate.resume) }) }
	t.Cleanup(unblock)
	failed := startInstallTransaction(gate, dir, []stagedBinary{
		{name: host, bin: []byte("failed release host")},
		{name: badName, bin: []byte("failed release codex")},
	})
	select {
	case <-gate.paused:
	case <-time.After(5 * time.Second):
		t.Fatal("failing install did not reach its second write")
	}
	succeeded := startInstallTransaction(io.Discard, dir, []stagedBinary{
		{name: host, bin: []byte("successful release host")},
		{name: "codex", bin: []byte("successful release codex")},
	})
	select {
	case err := <-succeeded:
		succeeded <- err
		t.Error("concurrent release completed before the failing transaction released its lock")
	case <-time.After(200 * time.Millisecond):
	}
	unblock()
	if err := transactionResult(t, failed); err == nil || !strings.Contains(err.Error(), badName) {
		t.Fatalf("second write error = %v; want failure for %s", err, badName)
	}
	if err := transactionResult(t, succeeded); err != nil {
		t.Fatal(err)
	}
	assertInstalledCodexBinary(t, host, "successful release host")
	assertInstalledCodexBinary(t, "codex", "successful release codex")
}

func startInstallTransaction(w io.Writer, dir string, all []stagedBinary) chan error {
	done, started := make(chan error, 1), make(chan struct{})
	go func() {
		close(started)
		done <- installBinaries(w, dir, all)
	}()
	<-started
	return done
}

type transactionGate struct {
	paused chan struct{}
	resume chan struct{}
	writes int
}

func (g *transactionGate) Write(p []byte) (int, error) {
	g.writes++
	if g.writes == 2 {
		close(g.paused)
		<-g.resume
	}
	return len(p), nil
}

func transactionResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("install transaction did not finish")
		return nil
	}
}

func TestInstallTailscaleDryRunPerOS(t *testing.T) {
	setupToolsTestEnv(t)
	old := goosOverride
	t.Cleanup(func() { goosOverride = old })
	for goos, want := range map[string]string{
		"darwin": "brew install --cask tailscale-app",
		"linux":  tailscaleInstallScript,
	} {
		goosOverride = goos
		var buf bytes.Buffer
		if err := installTailscale(context.Background(), &buf, t.TempDir(), true); err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		if !strings.Contains(buf.String(), want) {
			t.Errorf("%s output = %q, want %q", goos, buf.String(), want)
		}
	}
	goosOverride = "windows"
	if err := installTailscale(context.Background(), &bytes.Buffer{}, "", true); err == nil || !strings.Contains(err.Error(), "tailscale.com/download") {
		t.Errorf("windows err = %v, want download URL", err)
	}
}

// The macOS app puts no tailscale on PATH: the wrapper must run the app's CLI with the caller's args, and an existing file is kept.
func TestLinkTailscaleCLIWrapsAppBinary(t *testing.T) {
	setupToolsTestEnv(t)
	app := filepath.Join(t.TempDir(), "Tailscale")
	if err := os.WriteFile(app, []byte("#!/bin/sh\necho app \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := tailscaleAppBin
	tailscaleAppBin = app
	t.Cleanup(func() { tailscaleAppBin = old })
	if err := linkTailscaleCLI(&bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(localBinDir(), "tailscale")
	out, err := exec.Command(dst, "status", "--json").Output()
	if err != nil || string(out) != "app status --json\n" {
		t.Fatalf("wrapper ran %q err=%v, want the app CLI with the args", out, err)
	}
	var buf bytes.Buffer
	if err := linkTailscaleCLI(&buf, false); err != nil || !strings.Contains(buf.String(), "keep") {
		t.Fatalf("second run = %q err=%v, want keep", buf.String(), err)
	}
}

func TestTailscaleRepairsDanglingCLI(t *testing.T) {
	setupToolsTestEnv(t)
	app := filepath.Join(t.TempDir(), "Tailscale")
	writeToolsFixture(t, app, "#!/bin/sh\nprintf 'app %s\\n' \"$1\"\n", 0o755)
	old := tailscaleAppBin
	tailscaleAppBin = app
	t.Cleanup(func() { tailscaleAppBin = old })
	dst := filepath.Join(localBinDir(), "tailscale")
	symlinkToolsFixture(t, filepath.Join(t.TempDir(), "missing"), dst)

	var out bytes.Buffer
	if err := linkTailscaleCLI(&out, false); err != nil {
		t.Fatal(err)
	}
	got, err := exec.Command(dst, "status").Output()
	if err != nil || string(got) != "app status\n" {
		t.Fatalf("repaired CLI output = %q, error = %v; want app status", got, err)
	}
	if strings.Contains(out.String(), "keep") {
		t.Fatalf("dangling CLI was reported as kept: %s", &out)
	}
}
