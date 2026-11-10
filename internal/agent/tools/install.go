package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/edotau/claude-code/internal/agent"
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
	agyInstallScriptURL      = "https://antigravity.google/cli/install.sh"
	tailscaleInstallScript   = "https://tailscale.com/install.sh"
)

// installCmdTimeout bounds each installer subprocess so a hung prompt or network never blocks the command forever.
const installCmdTimeout = 10 * time.Minute

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
	if resp.StatusCode/100 != 2 {
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
	ctx, cancel := context.WithTimeout(ctx, installCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{f.Name()}, args...)...)
	cmd.Env = agent.EnvSpec{Mode: agent.EnvAgent, Set: env}.Environ(ctx) // a downloaded script gets no provider credential
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
	if dryRun {
		return installOpencodeLocked(ctx, w, dir, src, dryRun)
	}
	return paths.WithFileLock(src, func() error { return installOpencodeLocked(ctx, w, dir, src, dryRun) })
}

// installOpencodeLocked is installOpencode's body, run holding the cross-process lock on src.
func installOpencodeLocked(ctx context.Context, w io.Writer, dir, src string, dryRun bool) error {
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
	return installBinaryLocked(w, dir, "opencode", data)
}

// restoreOpencodeAside undoes a refused/failed install and returns cause, joined with any restore failure.
func restoreOpencodeAside(src, aside string, cause error) error {
	if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
		cause = errors.Join(cause, fmt.Errorf("opencode: remove refused binary %s: %w", src, err))
	}
	if _, err := os.Stat(aside); err != nil {
		return cause
	}
	if err := os.Rename(aside, src); err != nil {
		return errors.Join(cause, fmt.Errorf("opencode: restore previous binary %s: %w", aside, err))
	}
	return cause
}

// refuseOpencodeMajor blocks linking bin when its major exceeds opencodeMaxMajor; a failed or unparsable version probe is refused too.
func refuseOpencodeMajor(ctx context.Context, w io.Writer, bin string) error {
	if os.Getenv(opencodeAllowMajorEnv) != "" {
		return nil
	}
	version := FirstVersionLine(ctx, bin, versionProbeTimeout)
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

// claudeScienceAssetName mirrors claude.ai/install.sh's os/arch naming (x64/arm64).
func claudeScienceAssetName(goos, goarch string) string {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	if arch == "" {
		return ""
	}
	switch goos {
	case "linux":
		return "linux-" + arch
	case "darwin":
		return "mac-" + arch + ".dmg"
	default:
		return ""
	}
}

// installClaudeScience installs the macOS app from its DMG, or the raw Linux binary, for this platform.
func installClaudeScience(ctx context.Context, w io.Writer, dir string, dryRun bool) error {
	asset := claudeScienceAssetName(goosOverride, goarchOverride)
	if asset == "" {
		return fmt.Errorf("claude-science: no download mapped for %s/%s", goosOverride, goarchOverride)
	}
	url := claudeScienceBaseURL + "/" + asset
	if goosOverride == "darwin" {
		return installScienceApp(ctx, w, dir, url, dryRun)
	}
	fmt.Fprintf(w, "  download %s\n", url)
	if dryRun {
		fmt.Fprintln(w, "  (dry-run: would download and install claude-science)")
		return nil
	}
	data, err := httpGet(ctx, url)
	if err != nil {
		return err
	}
	return installBinaryLocked(w, dir, "claude-science", data)
}

// runSelfUpdate prints and runs `<bin> update`, skipping the exec on dry-run.
func runSelfUpdate(ctx context.Context, w io.Writer, bin string, dryRun bool) error {
	fmt.Fprintf(w, "  exec     %s update\n", bin)
	if dryRun {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, installCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "update")
	cmd.Env = agent.EnvSpec{Mode: agent.EnvAgent}.Environ(ctx) // the updater gets no provider credential
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

// installAgy runs Google's installer into ~/.local/bin, where Link finds agy for bin/gemini; dir/agy would go unlinked.
func installAgy(ctx context.Context, w io.Writer, _ string, dryRun bool) error {
	local := localBinDir()
	if local == "" {
		return errors.New("agy: no home directory for ~/.local/bin")
	}
	return runInstallScript(ctx, w, dryRun, agyInstallScriptURL, []string{"--dir", local}, nil)
}

// updateAgy runs the installed binary's own updater; a missing binary falls back to a fresh install.
func updateAgy(ctx context.Context, w io.Writer, dir string, dryRun bool) error {
	bin, err := paths.AgentBin("agy")
	if err != nil {
		bin, err = paths.LookPathReal("agy")
	}
	if err != nil {
		return installAgy(ctx, w, dir, dryRun)
	}
	return runSelfUpdate(ctx, w, bin, dryRun)
}
