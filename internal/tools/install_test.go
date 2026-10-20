package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

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

func TestClaudeScienceAssetName(t *testing.T) {
	setupToolsTestEnv(t)
	for _, tc := range []struct{ goos, goarch, want string }{
		{"linux", "amd64", "linux-x64"},
		{"darwin", "arm64", "darwin-arm64"},
		{"windows", "amd64", ""},
	} {
		if got := claudeScienceAssetName(tc.goos, tc.goarch); got != tc.want {
			t.Errorf("claudeScienceAssetName(%s,%s) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
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
	const installed = "#!/bin/sh\nprintf 'fake opencode\\n'\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`#!/bin/sh
mkdir -p "$HOME/.opencode/bin"
cat > "$HOME/.opencode/bin/opencode" <<'EOF'
#!/bin/sh
printf 'fake opencode\n'
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
