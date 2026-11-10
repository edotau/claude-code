package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
