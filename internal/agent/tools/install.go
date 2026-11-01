package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// Install script URLs, verified by downloading and reading each one (callers below pass the env/args each needs).
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

// runInstallScript downloads url to a temp file and runs `bash <file> [args...]` with env added, never piped to sh.
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
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
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

// installClaude runs Anthropic's installer, which itself lands in ~/.local/share/claude/versions + ~/.local/bin/claude.
func installClaude(ctx context.Context, w io.Writer, _ string, dryRun bool) error {
	return runInstallScript(ctx, w, dryRun, claudeInstallScriptURL, nil, nil)
}

// redirectToBinDir logs why the install moves to paths.BinDir() and returns it.
func redirectToBinDir(w io.Writer, reason string) string {
	corrected := paths.BinDir()
	fmt.Fprintf(w, "  into     %s (%s; installing into BinDir instead)\n", corrected, reason)
	return corrected
}

// installCopilot sets PREFIX=filepath.Dir(dir) so it lands in dir; a non-bin/ dir installs to paths.BinDir().
func installCopilot(ctx context.Context, w io.Writer, dir string, dryRun bool) error {
	if filepath.Base(dir) != "bin" {
		dir = redirectToBinDir(w, "not a bin/ dir")
	}
	return runInstallScript(ctx, w, dryRun, copilotInstallScriptURL, nil, []string{"PREFIX=" + filepath.Dir(dir)})
}

// opencodeMaxMajor is the highest opencode major version claude-code will link into bin/; upstream's
// installer always fetches latest, and 2.x broke the harness opencode route (memory id:ocnodry,
// 2026-10-20) — bump this once 2.x is verified compatible, don't just delete the check.
const opencodeMaxMajor = 1

// opencodeAllowMajorEnv, set non-empty, lifts the opencodeMaxMajor refusal for one install.
const opencodeAllowMajorEnv = "HARNESS_ALLOW_OPENCODE_2X"

// installOpencode runs upstream's installer, refuses a major above opencodeMaxMajor, then copies into dir.
func installOpencode(ctx context.Context, w io.Writer, dir string, dryRun bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	src := filepath.Join(home, ".opencode", "bin", "opencode")
	aside := src + ".prev"

	if !dryRun {
		if _, statErr := os.Stat(src); statErr == nil {
			if err := os.Rename(src, aside); err != nil {
				return fmt.Errorf("opencode: aside existing binary: %w", err)
			}
		}
	}

	if err := runInstallScript(ctx, w, dryRun, opencodeInstallScriptURL, []string{"--no-modify-path"}, nil); err != nil {
		return restoreOpencodeAside(src, aside, err)
	}
	if dryRun {
		fmt.Fprintf(w, "  (dry-run: would copy ~/.opencode/bin/opencode into %s, refusing major > %d unless %s is set)\n",
			dir, opencodeMaxMajor, opencodeAllowMajorEnv)
		return nil
	}
	if err := refuseOpencodeMajor(ctx, w, src); err != nil {
		return restoreOpencodeAside(src, aside, err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return restoreOpencodeAside(src, aside, fmt.Errorf("opencode: installed but %s unreadable: %w", src, err))
	}
	defer func() { _ = os.Remove(aside) }() // src now holds an accepted version; the aside copy is no longer needed
	return installBinary(w, dir, "opencode", data)
}

// restoreOpencodeAside undoes a refused/failed install and returns cause, joined with any restore failure.
func restoreOpencodeAside(src, aside string, cause error) error {
	_ = os.Remove(src)
	if _, err := os.Stat(aside); err != nil {
		return cause
	}
	if err := os.Rename(aside, src); err != nil {
		return errors.Join(cause, fmt.Errorf("opencode: restore previous binary %s: %w", aside, err))
	}
	return cause
}

// refuseOpencodeMajor blocks linking bin when its major exceeds opencodeMaxMajor; a probe failure is not blocked.
func refuseOpencodeMajor(ctx context.Context, w io.Writer, bin string) error {
	if os.Getenv(opencodeAllowMajorEnv) != "" {
		return nil
	}
	version := firstVersionLineCtx(ctx, bin)
	major := textutil.VersionMajor(version)
	if major == 0 {
		fmt.Fprintf(w, "  refuse   opencode %q (version unknown)\n", version)
		return fmt.Errorf("opencode version probe of %s failed or unparsable; refusing (set %s=1 to accept)", bin, opencodeAllowMajorEnv)
	}
	if major <= opencodeMaxMajor {
		return nil
	}
	fmt.Fprintf(w, "  refuse   opencode %s (major %d > %d)\n", version, major, opencodeMaxMajor)
	return fmt.Errorf("opencode installer fetched %s; harness supports <%d.0 (set %s=1 to accept)",
		version, opencodeMaxMajor+1, opencodeAllowMajorEnv)
}

// ghRelease is the subset of a GitHub releases/latest response this package reads.
type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

// ghAsset is one downloadable file of a release.
type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// codexAssetName is the codex release asset for goos/goarch (musl on Linux), or "" when unmapped.
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
	if i := slices.IndexFunc(release.Assets, func(a ghAsset) bool { return a.Name == name }); i >= 0 {
		return release.Assets[i].URL, name, nil
	}
	return "", "", fmt.Errorf("codex: asset %s not found in release %s", name, release.TagName)
}

// extractTarGzSingleFile returns the bytes of the one regular file in a .tar.gz, the shape of every codex asset.
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

// codexCompanions ship as their own release assets beside codex; 0.163+ refuses tool calls without its host.
var codexCompanions = []string{"codex-code-mode-host"}

// installCodex extracts the latest release and its companions into BinDir(), never another dir.
func installCodex(ctx context.Context, w io.Writer, dir string, dryRun bool) error {
	if dir != paths.BinDir() {
		dir = redirectToBinDir(w, "codex ships real release binaries")
	}
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
		fmt.Fprintln(w, "  (dry-run: would download and extract codex + "+strings.Join(codexCompanions, ", ")+")")
		return nil
	}
	if err := installReleaseBinary(ctx, w, dir, "codex", dlURL); err != nil {
		return err
	}
	for _, name := range codexCompanions {
		asset := strings.Replace(assetName, "codex", name, 1)
		i := slices.IndexFunc(rel.Assets, func(a ghAsset) bool { return a.Name == asset })
		if i < 0 {
			return fmt.Errorf("codex: companion asset %s not found in release %s", asset, rel.TagName)
		}
		if err := installReleaseBinary(ctx, w, dir, name, rel.Assets[i].URL); err != nil {
			return err
		}
	}
	return nil
}

// installReleaseBinary downloads one single-file .tar.gz release asset and installs it as name.
func installReleaseBinary(ctx context.Context, w io.Writer, dir, name, url string) error {
	tgz, err := httpGet(ctx, url)
	if err != nil {
		return err
	}
	bin, err := extractTarGzSingleFile(tgz)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return installBinary(w, dir, name, bin)
}

// claudeScienceAssetName mirrors claude.ai/install.sh's os/arch naming (x64/arm64).
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
func installClaudeScience(ctx context.Context, w io.Writer, dir string, dryRun bool) error {
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
	return installBinary(w, dir, "claude-science", data)
}

// runSelfUpdate prints and runs `<bin> update`, skipping the exec on dry-run.
func runSelfUpdate(ctx context.Context, w io.Writer, bin string, dryRun bool) error {
	fmt.Fprintf(w, "  exec     %s update\n", bin)
	if dryRun {
		return nil
	}
	cmd := exec.CommandContext(ctx, bin, "update")
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

// updateClaudeScience runs the installed binary's own updater; a missing binary falls back to a fresh install.
func updateClaudeScience(ctx context.Context, w io.Writer, dir string, dryRun bool) error {
	bin, err := paths.LookPathReal("claude-science")
	if err != nil {
		return installClaudeScience(ctx, w, dir, dryRun)
	}
	return runSelfUpdate(ctx, w, bin, dryRun)
}

// updateAgy runs the installed binary's own updater; agy has no known fresh-install path (Hint covers that).
func updateAgy(ctx context.Context, w io.Writer, dir string, dryRun bool) error {
	bin, err := paths.AgentBin("agy")
	if err != nil {
		bin, err = paths.LookPathReal("agy")
	}
	if err != nil {
		return errors.New("agy: not on PATH; no known install URL (claude-code configure signs it in)")
	}
	return runSelfUpdate(ctx, w, bin, dryRun)
}
