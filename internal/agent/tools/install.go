package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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
	agyInstallScriptURL      = "https://antigravity.google/cli/install.sh"
	tailscaleInstallScript   = "https://tailscale.com/install.sh"
	tailscaleStaticURL       = "https://pkgs.tailscale.com/stable/"
)

// installCmdTimeout bounds each installer subprocess so a hung prompt or network never blocks the command forever.
const installCmdTimeout = 10 * time.Minute

// installWaitDelay is how long Wait lingers after the kill for a grandchild holding the output pipe.
const installWaitDelay = 5 * time.Second

var httpClient = &http.Client{Timeout: 3 * time.Minute}

// goosOverride/goarchOverride pick the codex asset; vars so tests can select another platform.
var goosOverride, goarchOverride = runtime.GOOS, runtime.GOARCH

// maxDownload caps one installer download; a var so tests can lower it.
var maxDownload int64 = 512 << 20

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
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err == nil && int64(len(b)) > maxDownload {
		return nil, fmt.Errorf("%s: response exceeds %d bytes", url, maxDownload)
	}
	return b, err
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
	return runLogged(ctx, w, env, "bash", append([]string{f.Name()}, args...)...)
}

// runLogged runs name under installCmdTimeout with a credential-free agent env plus env, streaming output to w.
func runLogged(ctx context.Context, w io.Writer, env []string, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, installCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = agent.EnvSpec{Mode: agent.EnvAgent, Set: env}.Environ(agent.ProbeContext(ctx, "")) // no credential or login
	cmd.Stdout, cmd.Stderr = w, w
	cmd.WaitDelay = installWaitDelay
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

// opencodeMaxMajor caps the opencode major linked into bin/ (2.x broke the route, id:ocnodry); bump once verified.
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

// refuseOpencodeMajor blocks linking bin when its major exceeds opencodeMaxMajor; an unparsable probe is refused too.
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

// runSelfUpdate prints and runs `<bin> update`, skipping the exec on dry-run.
func runSelfUpdate(ctx context.Context, w io.Writer, bin string, dryRun bool) error {
	fmt.Fprintf(w, "  exec     %s update\n", bin)
	if dryRun {
		return nil
	}
	return runLogged(ctx, w, nil, bin, "update")
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
	bin, err := io.ReadAll(io.LimitReader(tr, maxDownload+1))
	if err != nil {
		return nil, err
	}
	if int64(len(bin)) > maxDownload {
		return nil, fmt.Errorf("archive entry %s exceeds %d bytes", hdr.Name, maxDownload)
	}
	rest, err := io.Copy(io.Discard, io.LimitReader(gz, maxDownload+1)) // reading to EOF verifies the gzip checksum
	if err != nil {
		return nil, fmt.Errorf("gzip stream: %w", err)
	}
	if rest > maxDownload {
		return nil, fmt.Errorf("archive tail exceeds %d bytes", maxDownload)
	}
	return bin, nil
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
	all, err := stageCodex(ctx, rel, assetName, dlURL)
	if err != nil {
		return err
	}
	return installBinaries(w, dir, all)
}

type stagedBinary struct {
	name string
	bin  []byte
}

// priorFile is a target's content before a release swap; existed=false means the file was new.
type priorFile struct {
	name    string
	data    []byte
	existed bool
}

// installBinaries writes all into dir as one locked transaction; a failed write restores the replaced files.
func installBinaries(w io.Writer, dir string, all []stagedBinary) error {
	return withBinLock(dir, func() error {
		prior := make([]priorFile, 0, len(all))
		for _, s := range all {
			data, err := os.ReadFile(filepath.Join(dir, s.name))
			if err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("snapshot %s: %w", s.name, err)
			}
			prior = append(prior, priorFile{s.name, data, err == nil})
		}
		for i, s := range all {
			if err := installBinary(w, dir, s.name, s.bin); err != nil {
				return errors.Join(err, rollbackBinaries(dir, prior[:i]))
			}
		}
		return nil
	})
}

// rollbackBinaries restores each file to its snapshot, removing ones that did not exist before; lock held by caller.
func rollbackBinaries(dir string, done []priorFile) error {
	var errs []error
	for _, p := range done {
		dest := filepath.Join(dir, p.name)
		if !p.existed {
			errs = append(errs, os.Remove(dest))
			continue
		}
		errs = append(errs, installBinary(io.Discard, dir, p.name, p.data))
	}
	return errors.Join(errs...)
}

// stageCodex downloads every asset before writing any, so a failure leaves the old install untouched.
func stageCodex(ctx context.Context, rel ghRelease, assetName, dlURL string) ([]stagedBinary, error) {
	var all []stagedBinary
	codexBin, err := fetchReleaseBinary(ctx, "codex", dlURL)
	if err != nil {
		return nil, err
	}
	for _, name := range codexCompanions {
		asset := strings.Replace(assetName, "codex", name, 1)
		i := slices.IndexFunc(rel.Assets, func(a ghAsset) bool { return a.Name == asset })
		if i < 0 {
			return nil, fmt.Errorf("codex: companion asset %s not found in release %s", asset, rel.TagName)
		}
		bin, err := fetchReleaseBinary(ctx, name, rel.Assets[i].URL)
		if err != nil {
			return nil, err
		}
		all = append(all, stagedBinary{name, bin})
	}
	return append(all, stagedBinary{"codex", codexBin}), nil
}

// fetchReleaseBinary downloads one single-file .tar.gz release asset and returns the binary inside.
func fetchReleaseBinary(ctx context.Context, name, url string) ([]byte, error) {
	tgz, err := httpGet(ctx, url)
	if err != nil {
		return nil, err
	}
	bin, err := extractTarGzSingleFile(tgz)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return bin, nil
}

// tailscaleAppBin is the CLI inside the macOS app; the cask puts nothing on PATH.
var tailscaleAppBin = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// installTailscaleCask runs `brew install --cask tailscale-app`; brew itself skips an installed app.
func installTailscaleCask(ctx context.Context, w io.Writer, dryRun bool) error {
	fmt.Fprintln(w, "  exec     brew install --cask tailscale-app")
	if dryRun {
		return nil
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		return errors.New("tailscale: brew not found; install Homebrew or see https://tailscale.com/download")
	}
	return runLogged(ctx, w, nil, brew, "install", "--cask", "tailscale-app")
}

// linkTailscaleCLI writes ~/.local/bin/tailscale exec'ing the app's CLI (Tailscale's documented alias, no admin).
func linkTailscaleCLI(w io.Writer, dryRun bool) error {
	local := localBinDir()
	if local == "" {
		return errors.New("tailscale: no home directory for ~/.local/bin")
	}
	dst := filepath.Join(local, "tailscale")
	if _, err := os.Stat(dst); err == nil { // Stat follows links: a dangling link is rewritten, not kept
		fmt.Fprintln(w, "  keep     "+dst)
		return nil
	}
	fmt.Fprintf(w, "  cli      %s → %s\n", dst, tailscaleAppBin)
	if dryRun {
		return nil
	}
	if _, err := os.Stat(tailscaleAppBin); err != nil {
		return fmt.Errorf("tailscale: app CLI missing after install: %w", err)
	}
	return installBinaryLocked(w, local, "tailscale", []byte("#!/bin/sh\nexec \""+tailscaleAppBin+"\" \"$@\"\n"))
}

// tailscaleNoRoot says why the official script cannot run (not uid 0, no sudo or doas), or "" when it can; a test seam.
var tailscaleNoRoot = func() string {
	if os.Geteuid() == 0 {
		return ""
	}
	for _, c := range []string{"sudo", "doas"} {
		if _, err := exec.LookPath(c); err == nil {
			return ""
		}
	}
	return "sudo and doas not found"
}

// tailscaleStaticBins are the only archive members the no-root install keeps, in write order.
var tailscaleStaticBins = []string{"tailscale", "tailscaled"}

// tarWanted is hdr's base name when it is a regular file in want, local to the archive, at most one directory deep.
func tarWanted(hdr *tar.Header, want []string) (string, bool) {
	clean := path.Clean(hdr.Name)
	local := !path.IsAbs(clean) && clean != ".." && !strings.HasPrefix(clean, "../")
	base := path.Base(clean)
	return base, hdr.Typeflag == tar.TypeReg && local && strings.Count(clean, "/") <= 1 && slices.Contains(want, base)
}

// extractTarGzFiles returns the regular files named in want at the archive top level or one directory down.
func extractTarGzFiles(data []byte, want []string) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr, got := tar.NewReader(gz), map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return got, nil
		}
		if err != nil {
			return nil, err
		}
		base, ok := tarWanted(hdr, want)
		if !ok {
			continue
		}
		bin, err := io.ReadAll(io.LimitReader(tr, maxDownload+1))
		if err != nil {
			return nil, err
		}
		if int64(len(bin)) > maxDownload {
			return nil, fmt.Errorf("archive entry %s exceeds %d bytes", hdr.Name, maxDownload)
		}
		got[base] = bin
	}
}

// fetchTailscaleStatic downloads the tarball, verifies its .sha256 and returns tailscale and tailscaled.
func fetchTailscaleStatic(ctx context.Context, url string) ([]stagedBinary, error) {
	tgz, err := httpGet(ctx, url)
	if err != nil {
		return nil, err
	}
	sumFile, err := httpGet(ctx, url+".sha256")
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(tgz)
	if f := strings.Fields(string(sumFile)); len(f) == 0 || !strings.EqualFold(f[0], hex.EncodeToString(sum[:])) {
		return nil, fmt.Errorf("tailscale: %s sha256 does not match %s.sha256; nothing written", url, url)
	}
	files, err := extractTarGzFiles(tgz, tailscaleStaticBins)
	if err != nil {
		return nil, fmt.Errorf("tailscale: %w", err)
	}
	out := make([]stagedBinary, 0, len(tailscaleStaticBins))
	for _, name := range tailscaleStaticBins {
		bin, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("tailscale: %s not found in %s", name, url)
		}
		out = append(out, stagedBinary{name, bin})
	}
	return out, nil
}

// tailscaleStaticAsset is the static tarball URL for goarchOverride from Tailscale's package index.
func tailscaleStaticAsset(ctx context.Context) (string, error) {
	if goarchOverride != "amd64" && goarchOverride != "arm64" {
		return "", fmt.Errorf("tailscale: no static build mapped for %s", goarchOverride)
	}
	raw, err := httpGet(ctx, tailscaleStaticURL+"?mode=json")
	if err != nil {
		return "", err
	}
	var index struct{ Tarballs map[string]string }
	if err := json.Unmarshal(raw, &index); err != nil {
		return "", fmt.Errorf("tailscale: package index %w", err)
	}
	name := index.Tarballs[goarchOverride]
	if name == "" || path.Base(name) != name {
		return "", fmt.Errorf("tailscale: no usable %s tarball in the package index", goarchOverride)
	}
	return tailscaleStaticURL + name, nil
}

// installTailscaleStatic puts the static tarball's binaries in ~/.local/bin; reason is why there is no root.
func installTailscaleStatic(ctx context.Context, w io.Writer, dryRun bool, reason string) error {
	local := localBinDir()
	if local == "" {
		return errors.New("tailscale: no home directory for ~/.local/bin")
	}
	url, err := tailscaleStaticAsset(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "  static   %s (no root: %s)\n", url, reason)
	if dryRun {
		fmt.Fprintf(w, "  (dry-run: would verify the sha256 and write %s into %s)\n", strings.Join(tailscaleStaticBins, ", "), local)
		return nil
	}
	bins, err := fetchTailscaleStatic(ctx, url)
	if err != nil {
		return err
	}
	if err := installBinaries(w, local, bins); err != nil {
		return err
	}
	dir := filepath.Join(paths.StateDir(), "tailscale")
	fmt.Fprintf(w, "  note     tailscaled is not started; as this user run: tailscaled --tun=userspace-networking"+
		" --state=%[1]s/tailscaled.state --socket=%[1]s/tailscaled.sock, then tailscale --socket=%[1]s/tailscaled.sock up\n", dir)
	return nil
}

// installTailscale is a system install, not into dir: brew cask on macOS; Linux the script, or static without root.
func installTailscale(ctx context.Context, w io.Writer, _ string, dryRun bool) error {
	switch goosOverride {
	case "linux":
		if reason := tailscaleNoRoot(); reason != "" {
			return installTailscaleStatic(ctx, w, dryRun, reason)
		}
		return runInstallScript(ctx, w, dryRun, tailscaleInstallScript, nil, nil)
	case "darwin":
		if err := installTailscaleCask(ctx, w, dryRun); err != nil {
			return err
		}
		return linkTailscaleCLI(w, dryRun)
	}
	return fmt.Errorf("tailscale: no installer for %s; see https://tailscale.com/download", goosOverride)
}
