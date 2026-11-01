package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// ShortHome replaces a leading home directory with ~, leaving sibling paths unchanged.
func ShortHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return p
}

// ExpandHome expands only ~ and a leading ~/ using the current home directory.
func ExpandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

// ConfigDirIsDefault reports whether the config directory is the current home's .claude directory.
func ConfigDirIsDefault() bool {
	home, err := os.UserHomeDir()
	return err == nil && filepath.Clean(ConfigDir()) == filepath.Join(home, ".claude")
}
