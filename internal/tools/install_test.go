package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCodexAssetName(t *testing.T) {
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
