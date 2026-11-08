package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

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
	all, err := stageCodex(ctx, rel, assetName, dlURL)
	if err != nil {
		return err
	}
	for _, s := range all {
		if err := installBinary(w, dir, s.name, s.bin); err != nil {
			return err
		}
	}
	return nil
}

type stagedBinary struct {
	name string
	bin  []byte
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
