package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Vendors maps bin names to shipped binaries; gemini is Google's agy.
var Vendors = map[string]string{
	"claude": "claude", "codex": "codex", "copilot": "copilot", "opencode": "opencode", "gemini": "agy",
}

// VendorName maps a bin name or a shipped binary ("agy") to its bin name.
func VendorName(name string) (string, bool) {
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

// VendorBin returns BinDir()/<bin> when it is a usable vendor executable, never its resolved target.
func VendorBin(name string) (string, error) {
	bin, ok := VendorName(name)
	if !ok {
		return "", fmt.Errorf("unknown vendor %q", name)
	}
	p := filepath.Join(BinDir(), bin)
	fi, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("%s: not installed at %s (claude-code install --tools=%s)", bin, p, Vendors[bin])
	}
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(p)
	if fi.Mode()&os.ModeSymlink != 0 && os.IsNotExist(err) {
		return "", fmt.Errorf("%s: %s is a broken link (run claude-code install)", bin, p)
	}
	if err != nil {
		return "", err
	}
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	if real == self || filepath.Base(real) == "claude-code" {
		return "", fmt.Errorf("%s: %s is the retired claude-code shim (run claude-code install)", bin, p)
	}
	fi, err = os.Stat(p)
	if err != nil {
		return "", err
	}
	if fi.IsDir() || fi.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%s: %s is not executable", bin, p)
	}
	return p, nil
}
