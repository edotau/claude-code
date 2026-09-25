// Package crossgen projects agents/ and skills/ into Gemini CLI's skill tree and renders AGENTS.md/GEMINI.md.
package crossgen

import (
	"os"
	"path/filepath"
	"strings"
)

// GeminiSkillsDir is where Gemini CLI reads user skills: $HOME/.gemini/skills ("" when HOME is unresolvable).
func GeminiSkillsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini", "skills")
}

// safeName accepts only a plain path component: no separators, no dot-prefix, no "." / "..".
func safeName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, `/\`) &&
		!strings.HasPrefix(name, ".")
}

// isDir reports whether p resolves to a directory (symlinks followed).
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// displayPath renders a path home-relative (~/...) when possible, else absolute.
func displayPath(path string) string {
	home, err := os.UserHomeDir()
	if err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + rel
		}
	}
	return path
}
