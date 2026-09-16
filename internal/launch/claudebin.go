package launch

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// SelfBinary is the resolved absolute path of the running harness binary.
func SelfBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Abs(exe)
}

// ClaudeBinary finds the real claude: PATH (skipping our shims), else the native installer's newest version.
func ClaudeBinary() (string, error) {
	if bin, err := paths.LookPathReal("claude"); err == nil {
		return bin, nil
	}
	if bin := newestInstalledClaude(); bin != "" {
		return bin, nil
	}
	return "", errors.New("claude: not found on PATH or under ~/.local/share/claude/versions (install: curl -fsSL https://claude.ai/install.sh | bash)")
}

func newestInstalledClaude() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".local", "share", "claude", "versions")
	entries, _ := os.ReadDir(dir)
	self, _ := SelfBinary()
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
