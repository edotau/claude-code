package claude

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// ClaudeBinary finds claude: BinDir/claude, else the native installer's newest (not under the bin-dir override).
func ClaudeBinary() (string, error) {
	bin, err := paths.AgentBin("claude")
	if err == nil {
		return bin, nil
	}
	if os.Getenv(paths.AgentBinDirEnv) != "" {
		return "", err // the override is hermetic: never fall back to the real install
	}
	if bin := newestInstalledClaude(); bin != "" {
		return bin, nil
	}
	return "", errors.New("claude: not found in " + paths.BinDir() +
		" or ~/.local/share/claude/versions (claude-code install --tools=claude)")
}

func newestInstalledClaude() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".local", "share", "claude", "versions")
	entries, _ := os.ReadDir(dir)
	self, _ := paths.SelfBinary()
	best := ""
	for _, e := range entries {
		cand := filepath.Join(dir, e.Name())
		fi, err := os.Stat(cand)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		if real, _ := filepath.EvalSymlinks(cand); real == self {
			continue
		}
		if best == "" || versionLess(filepath.Base(best), e.Name()) {
			best = cand
		}
	}
	return best
}

// versionLess compares dot-separated numeric fields (2.0.9 < 2.0.14).
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])
		if aerr != nil || berr != nil {
			if as[i] != bs[i] {
				return as[i] < bs[i]
			}
			continue
		}
		if an != bn {
			return an < bn
		}
	}
	return len(as) < len(bs)
}

// helperBin prefers the installed binary: the overlay file is shared, so a scratch build must never be baked in.
func helperBin(self string) string {
	if bin := filepath.Join(paths.BinDir(), "claude-code"); paths.IsFile(bin) {
		return bin
	}
	return self
}
