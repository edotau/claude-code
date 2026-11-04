package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Vendors maps bin names to shipped binaries; gemini is Google's agy.
var Vendors = map[string]string{
	"claude": "claude", "codex": "codex", "copilot": "copilot", "opencode": "opencode", "gemini": "agy",
}

// AgentName maps a bin name or a shipped binary ("agy") to its bin name.
func AgentName(name string) (string, bool) {
	if _, ok := Vendors[name]; ok {
		return name, true
	}
	for bin, vendor := range Vendors {
		if vendor == name {
			return bin, true
		}
	}
	return "", false
}

// AgentBin returns BinDir()/<bin> when it is a usable vendor executable, never its resolved target.
func AgentBin(name string) (string, error) {
	bin, ok := AgentName(name)
	if !ok {
		return "", fmt.Errorf("unknown vendor %q", name)
	}
	p := filepath.Join(BinDir(), bin)
	if err := checkVendorLink(bin, p); err != nil {
		return "", err
	}
	if err := checkVendorExec(bin, p); err != nil {
		return "", err
	}
	return p, nil
}

// checkVendorLink rejects a missing, broken or retired-shim entry at p.
func checkVendorLink(bin, p string) error {
	fi, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return fmt.Errorf("%s: not installed at %s (claude-code install --tools=%s)", bin, p, Vendors[bin])
	}
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(p)
	if fi.Mode()&os.ModeSymlink != 0 && os.IsNotExist(err) {
		return fmt.Errorf("%s: %s is a broken link (run claude-code install)", bin, p)
	}
	if err != nil {
		return err
	}
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	if real == self || filepath.Base(real) == "claude-code" {
		return fmt.Errorf("%s: %s is the retired claude-code shim (run claude-code install)", bin, p)
	}
	return nil
}

// checkVendorExec rejects a p that is not an executable regular file.
func checkVendorExec(bin, p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if fi.IsDir() || fi.Mode()&0o111 == 0 {
		return fmt.Errorf("%s: %s is not executable", bin, p)
	}
	return nil
}

// LookPathReal finds name on PATH skipping BinDir and any symlink to the running executable, so a
// `claude` shim never re-execs itself.
func LookPathReal(name string) (string, error) {
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	shimDir, _ := filepath.Abs(BinDir())
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if abs, _ := filepath.Abs(dir); abs == shimDir || dir == "" {
			continue
		}
		cand := filepath.Join(dir, name)
		fi, err := os.Stat(cand)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		if real, _ := filepath.EvalSymlinks(cand); self != "" && real == self {
			continue
		}
		return cand, nil
	}
	return "", errors.New(name + ": not found on PATH (outside " + shimDir + ")")
}
