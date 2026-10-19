package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// Install script URLs, verified by downloading and reading each one (see internal/tools/install.go
// callers below for the env/arg each needs to land in ~/.local/bin).
var (
	claudeInstallScriptURL   = "https://claude.ai/install.sh"
	copilotInstallScriptURL  = "https://gh.io/copilot-install"
	opencodeInstallScriptURL = "https://opencode.ai/install"
	codexReleaseURL          = "https://api.github.com/repos/openai/codex/releases/latest"
	claudeScienceBaseURL     = "https://downloads.claude.ai/claude-science/latest"
)

var httpClient = &http.Client{Timeout: 3 * time.Minute}

// goosOverride/goarchOverride pick the codex asset; vars so tests can select another platform.
var goosOverride, goarchOverride = runtime.GOOS, runtime.GOARCH

// httpGet fetches url and returns the body; non-2xx is an error.
func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "claude-code-tools")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// runInstallScript downloads url to a temp file and runs `bash <file> [args...]` under env added to
// the current environment — never `curl | sh`, so the script is on disk and readable before it runs.
func runInstallScript(ctx context.Context, w io.Writer, dryRun bool, url string, args, env []string) error {
	fmt.Fprintf(w, "  script   %s %v\n", url, args)
	if dryRun {
		fmt.Fprintln(w, "  (dry-run: would download and run the installer)")
		return nil
	}
	data, err := httpGet(ctx, url)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "claude-code-install-*.sh")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "bash", append([]string{f.Name()}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

// installClaude runs Anthropic's own installer, which lands under ~/.local/share/claude/versions and
// symlinks ~/.local/bin/claude itself (see internal/launch.ClaudeBinary) — no env knob needed.
func installClaude(ctx context.Context, w io.Writer, dryRun bool) error {
	return runInstallScript(ctx, w, dryRun, claudeInstallScriptURL, nil, nil)
}

// installCopilot forces PREFIX=$HOME/.local so the installer's own default (root -> /usr/local) never
// applies; its "bin" subdir is where the copilot binary lands.
func installCopilot(ctx context.Context, w io.Writer, dryRun bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return runInstallScript(ctx, w, dryRun, copilotInstallScriptURL, nil, []string{"PREFIX=" + filepath.Join(home, ".local")})
}

// installOpencode runs the upstream installer (no env knob exists — INSTALL_DIR is hardcoded to
// $HOME/.opencode/bin) with --no-modify-path so it never edits shell rc files, then copies the
// resulting binary into ~/.local/bin alongside every other tool here.
func installOpencode(ctx context.Context, w io.Writer, dryRun bool) error {
	if err := runInstallScript(ctx, w, dryRun, opencodeInstallScriptURL, []string{"--no-modify-path"}, nil); err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintln(w, "  (dry-run: would copy ~/.opencode/bin/opencode into ~/.local/bin)")
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	src := filepath.Join(home, ".opencode", "bin", "opencode")
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("opencode: installed but %s unreadable: %w", src, err)
	}
	return installBinary(w, "opencode", data)
}

// ghRelease is the subset of a GitHub releases/latest response this package reads.
type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// codexAssetName is the exact codex release asset for goos/goarch (musl on Linux: no glibc version
// dependency), or "" when this package has no mapping for that platform.
func codexAssetName(goos, goarch string) string {
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[goarch]
	if arch == "" {
		return ""
	}
	switch goos {
	case "linux":
		return "codex-" + arch + "-unknown-linux-musl.tar.gz"
	case "darwin":
		return "codex-" + arch + "-apple-darwin.tar.gz"
	default:
		return ""
	}
}

// selectCodexAsset finds the download URL for goos/goarch among release's assets.
func selectCodexAsset(release ghRelease, goos, goarch string) (url, name string, err error) {
	name = codexAssetName(goos, goarch)
	if name == "" {
		return "", "", fmt.Errorf("codex: no release asset mapped for %s/%s", goos, goarch)
	}
	for _, a := range release.Assets {
		if a.Name == name {
			return a.URL, name, nil
		}
	}
	return "", "", fmt.Errorf("codex: asset %s not found in release %s", name, release.TagName)
}

// extractTarGzSingleFile reads a .tar.gz holding exactly one regular file and returns its bytes —
// the shape every codex release asset uses (no directory, no second entry).
func extractTarGzSingleFile(data []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("empty archive: %w", err)
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil, fmt.Errorf("first entry %s is not a regular file", hdr.Name)
	}
	return io.ReadAll(tr)
}

// installCodex resolves the latest codex release, downloads its linux/darwin binary asset, and
// extracts the single file it contains into ~/.local/bin.
func installCodex(ctx context.Context, w io.Writer, dryRun bool) error {
	fmt.Fprintf(w, "  github   %s\n", codexReleaseURL)
	data, err := httpGet(ctx, codexReleaseURL)
	if err != nil {
		return err
	}
	var rel ghRelease
	if err := json.Unmarshal(data, &rel); err != nil {
		return fmt.Errorf("codex: release %w", err)
	}
	dlURL, assetName, err := selectCodexAsset(rel, goosOverride, goarchOverride)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "  release  %s %s\n", rel.TagName, assetName)
	if dryRun {
		fmt.Fprintln(w, "  (dry-run: would download and extract codex)")
		return nil
	}
	tgz, err := httpGet(ctx, dlURL)
	if err != nil {
		return err
	}
	bin, err := extractTarGzSingleFile(tgz)
	if err != nil {
		return fmt.Errorf("codex: %w", err)
	}
	return installBinary(w, "codex", bin)
}

// claudeScienceAssetName mirrors claude.ai/install.sh's own os/arch naming (x64/arm64), verified by
// downloading https://downloads.claude.ai/claude-science/latest/linux-x64 (150MB ELF binary).
func claudeScienceAssetName(goos, goarch string) string {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	if arch == "" {
		return ""
	}
	switch goos {
	case "linux", "darwin":
		return goos + "-" + arch
	default:
		return ""
	}
}

// installClaudeScience downloads the direct binary for this platform and installs it atomically.
func installClaudeScience(ctx context.Context, w io.Writer, dryRun bool) error {
	asset := claudeScienceAssetName(runtime.GOOS, runtime.GOARCH)
	if asset == "" {
		return fmt.Errorf("claude-science: no download mapped for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	url := claudeScienceBaseURL + "/" + asset
	fmt.Fprintf(w, "  download %s\n", url)
	if dryRun {
		fmt.Fprintln(w, "  (dry-run: would download and install claude-science)")
		return nil
	}
	data, err := httpGet(ctx, url)
	if err != nil {
		return err
	}
	return installBinary(w, "claude-science", data)
}

// updateClaudeScience runs the installed binary's own updater; a missing binary falls back to a fresh install.
func updateClaudeScience(ctx context.Context, w io.Writer, dryRun bool) error {
	bin, err := paths.LookPathReal("claude-science")
	if err != nil {
		return installClaudeScience(ctx, w, dryRun)
	}
	fmt.Fprintf(w, "  exec     %s update\n", bin)
	if dryRun {
		return nil
	}
	cmd := exec.CommandContext(ctx, bin, "update")
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

// updateAgy runs the installed binary's own updater; agy has no known fresh-install path (Hint covers that).
func updateAgy(ctx context.Context, w io.Writer, dryRun bool) error {
	bin, err := paths.LookPathReal("agy")
	if err != nil {
		return fmt.Errorf("agy: not on PATH; no known install URL (claude-code configure signs it in)")
	}
	fmt.Fprintf(w, "  exec     %s update\n", bin)
	if dryRun {
		return nil
	}
	cmd := exec.CommandContext(ctx, bin, "update")
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}
