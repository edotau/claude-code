package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
)

// ShimNames are the bare names install links to the harness binary.
var ShimNames = []string{"claude", "codex", "gemini", "opencode", "copilot"}

// SettingsFile is the live Claude Code settings.json under the config dir.
func SettingsFile() string { return filepath.Join(paths.ConfigDir(), "settings.json") }

// Install copies the binary into BinDir, links the shims, and merges the harness settings (backing up first).
func Install(w io.Writer, dryRun bool) error {
	self, err := selfBinary()
	if err != nil {
		return err
	}
	bin := paths.BinDir()
	target := filepath.Join(bin, "claude-code")
	if !sameFile(self, target) {
		fmt.Fprintf(w, "copy     %s → %s\n", self, target)
		if !dryRun {
			if err := copyBinary(self, target); err != nil {
				return err
			}
		}
	}
	for _, name := range ShimNames {
		link := filepath.Join(bin, name)
		if fi, err := os.Lstat(link); err == nil && fi.Mode()&fs.ModeSymlink == 0 {
			fmt.Fprintf(w, "skip     %s (a real file is there)\n", link)
			continue
		}
		fmt.Fprintf(w, "link     %s → claude-code\n", link)
		if !dryRun {
			_ = os.Remove(link)
			if err := os.Symlink("claude-code", link); err != nil {
				return err
			}
		}
	}
	file := SettingsFile()
	existing, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	rendered, err := settings.Render()
	if err != nil {
		return err
	}
	merged, sum, err := settings.Merge(existing, rendered)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "settings %s\n  keep   %s\n  merge  %s\n  hooks  %d harness + %d foreign kept\n",
		file, strings.Join(sum.Kept, ", "), strings.Join(sum.Owned, ", "), sum.HarnessHooks, sum.ForeignHooks)
	if string(merged) == string(existing) {
		fmt.Fprintln(w, "  unchanged")
		return nil
	}
	if dryRun {
		fmt.Fprintf(w, "  would write %d bytes (was %d); --dry-run wrote nothing\n", len(merged), len(existing))
		return nil
	}
	backup, err := settings.WriteWithBackup(file, merged)
	if backup != "" {
		fmt.Fprintf(w, "  backup %s\n", backup)
	}
	return err
}

// sameFile is true when target already is, or holds the same bytes as, self.
func sameFile(self, target string) bool {
	if real, _ := filepath.EvalSymlinks(target); real == self {
		return true
	}
	a, err1 := os.ReadFile(self)
	b, err2 := os.ReadFile(target)
	return err1 == nil && err2 == nil && bytes.Equal(a, b)
}

func copyBinary(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return paths.AtomicWrite(dst, data, 0o755)
}

// Doctor prints ✓/✗ checks and returns false on a hard failure.
func Doctor(w io.Writer) bool {
	ok := true
	check := func(pass, hard bool, format string, a ...any) {
		mark := "✓"
		if !pass {
			mark = "✗"
			if hard {
				ok = false
			}
		}
		fmt.Fprintf(w, "%s %s\n", mark, fmt.Sprintf(format, a...))
	}
	bin, err := ClaudeBinary()
	check(err == nil, true, "claude binary: %s", orErr(bin, err))
	reg, err := providers.Load()
	check(err == nil, true, "provider registry: %s", orErr(providers.UserFile(), err))
	if reg != nil {
		sel, serr := providers.Select(reg, "")
		for _, n := range reg.Names() {
			p := reg.Providers[n]
			session := serr == nil && sel.Provider == p
			label := p.Auth.Type
			if p.Auth.Env != "" {
				label += " " + p.Auth.Env
			}
			if session {
				label += " (session provider)"
			}
			check(providers.CredentialPresent(p), session, "credential %-12s %s", n, label)
		}
	}
	check(true, false, "router: %s", routerStatus())
	has, err := settingsHasHooks(SettingsFile())
	check(has, false, "settings.json harness hooks: %s", orErr(map[bool]string{true: "present", false: "missing (run claude-code install)"}[has], err))
	return ok
}

func routerStatus() string {
	b, err := os.ReadFile(filepath.Join(paths.StateDir(), "router.json"))
	if err != nil {
		return "not running"
	}
	var st struct {
		Port int `json:"port"`
	}
	if json.Unmarshal(b, &st) != nil || st.Port == 0 {
		return "router.json unreadable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", st.Port)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "stale router.json (port " + fmt.Sprint(st.Port) + " not answering)"
	}
	resp.Body.Close()
	return fmt.Sprintf("port %d %s", st.Port, resp.Status)
}

func settingsHasHooks(file string) (bool, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return false, err
	}
	var doc struct {
		Hooks map[string][]hookspec.Group `json:"hooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return false, err
	}
	for _, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if hookspec.IsHarness(h.Command) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func orErr(s string, err error) string {
	if err != nil {
		return err.Error()
	}
	return s
}
