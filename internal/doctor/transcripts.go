package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	secretredact "github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

var transcriptIdle = 10 * time.Minute

type transcriptStore struct {
	name, root string
	disabled   bool
	accept     func(string) bool
}

type transcriptCounts struct {
	scanned, found, matches, masked, maskedMatches, failed int
	bytes                                                  int64
	skipped                                                map[string]int
}

func transcriptStores() []transcriptStore {
	cfg := resolvedPath(paths.ConfigDir())
	home, _ := os.UserHomeDir()
	taskRoot := filepath.Join(os.TempDir(), fmt.Sprintf("claude-%d", os.Getuid()))
	task := func(p string) bool {
		return filepath.Ext(p) == ".output" && strings.Contains(filepath.ToSlash(p), "/tasks/")
	}
	text := func(p string) bool {
		return slices.Contains([]string{".jsonl", ".json", ".log", ".txt", ".md", ".output"}, filepath.Ext(p))
	}
	project := func(p string) bool {
		return filepath.Ext(p) == ".jsonl" || filepath.Ext(p) == ".output" || strings.Contains(filepath.ToSlash(p), "/tool-results/")
	}
	return []transcriptStore{
		{"claude projects", filepath.Join(cfg, "projects"), false, project},
		{"claude history", filepath.Join(cfg, "history.jsonl"), false, text},
		{"task outputs", filepath.Join(cfg, "tasks"), false, text},
		{"temporary task outputs", taskRoot, false, task},
		{"dispatch", filepath.Join(cfg, "state", "harness", "dispatch"), false, text},
		{"saved results", filepath.Join(cfg, "state", "harness", "results"), false, text},
		{"proxy log", filepath.Join(cfg, "state", "harness", "proxy.log"), false, text},
		{"reports", filepath.Join(cfg, "memory", "reports"), false, text},
		{"codex sessions", filepath.Join(home, ".codex", "sessions"), true, text},
		{"copilot sessions", filepath.Join(home, ".copilot", "session-state"), true, text},
	}
}

func transcriptMatcher() (*secretredact.Matcher, error) {
	return secretredact.Load(func() ([]string, error) {
		names, err := vault.NamesErr()
		if err != nil {
			return nil, err
		}
		records, err := vault.ReadSecrets(names)
		if err != nil {
			return nil, err
		}
		values := make([]string, 0, len(records))
		for _, value := range records {
			values = append(values, value)
		}
		return values, nil
	})
}

func transcriptAudit(w io.Writer, fix bool, targets []string) int {
	ew := &textutil.ErrWriter{W: w}
	fmt.Fprintln(ew, "\n== transcript stores ==")
	m, err := transcriptMatcher()
	rc := 0
	if err != nil {
		m = secretredact.FromValues(nil)
		fmt.Fprintln(ew, "  ⚠ vault unavailable: shapes only")
		if !errors.Is(err, vault.ErrLocked) {
			rc = 1
		}
	}
	fmt.Fprintf(ew, "  vault values skipped: %d\n", m.Skipped)
	for _, store := range transcriptStores() {
		counts := scanTranscriptStore(store, m, fix, targets)
		printTranscriptCounts(ew, store, counts)
		if counts.failed > 0 || (!fix && counts.matches > 0) {
			rc = 1
		}
	}
	fmt.Fprintln(ew, "  agy and OpenCode: skipped: SQLite out of scope")
	if ew.Err != nil {
		return 1
	}
	return rc
}

func printTranscriptCounts(w io.Writer, store transcriptStore, c transcriptCounts) {
	fmt.Fprintf(w, "  %s: scanned %d, with matches %d, matches %d, masked files %d, matches masked %d, bytes %d",
		store.name, c.scanned, c.found, c.matches, c.masked, c.maskedMatches, c.bytes)
	for _, reason := range []string{"not yet enabled", "symlink or outside root", "not idle", "would not stay valid", "error"} {
		if n := c.skipped[reason]; n > 0 || (reason == "not yet enabled" && store.disabled) {
			fmt.Fprintf(w, ", skipped: %s %d", reason, n)
		}
	}
	fmt.Fprintln(w)
}

func transcriptSelected(path string, targets []string) bool {
	if len(targets) == 0 {
		return true
	}
	for _, target := range targets {
		absolute, err := filepath.Abs(target)
		if err == nil && path == absolute {
			return true
		}
	}
	return false
}

func scanTranscriptStore(s transcriptStore, m *secretredact.Matcher, fix bool, targets []string) transcriptCounts {
	c := transcriptCounts{skipped: map[string]int{}}
	root, err := filepath.Abs(s.root)
	if err != nil {
		c.failed++
		c.skipped["error"]++
		return c
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) && path == root {
			return nil
		}
		if walkErr != nil {
			c.failed++
			c.skipped["error"]++
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			c.skipped["symlink or outside root"]++
			return nil
		}
		if d.IsDir() || !s.accept(path) || !transcriptSelected(path, targets) {
			return nil
		}
		scanTranscriptEntry(&c, s, path, d, m, fix)
		return nil
	})
	if err != nil {
		c.failed++
		c.skipped["error"]++
	}
	return c
}

func scanTranscriptEntry(c *transcriptCounts, s transcriptStore, path string, d fs.DirEntry, m *secretredact.Matcher, fix bool) {
	info, err := d.Info()
	if err != nil {
		c.failed++
		c.skipped["error"]++
		return
	}
	if !info.Mode().IsRegular() {
		return
	}
	if s.disabled {
		c.skipped["not yet enabled"]++
		c.bytes += info.Size()
		return
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		c.skipped["symlink or outside root"]++
		return
	}
	if fix && time.Since(info.ModTime()) < transcriptIdle {
		c.skipped["not idle"]++
		return
	}
	c.scanned++
	c.bytes += info.Size()
	hits, err := transcriptFile(path, m, fix)
	if err != nil {
		transcriptFailure(c, err)
		return
	}
	if hits > 0 {
		c.found++
		c.matches += hits
	}
	if fix && hits > 0 {
		c.masked++
		c.maskedMatches += hits
	}
}

func transcriptFailure(c *transcriptCounts, err error) {
	// The core currently exposes validation failure only as a sanitized error string.
	if strings.Contains(err.Error(), "invalid changed JSON line") || errors.Is(err, errTranscriptInvalid) {
		c.skipped["would not stay valid"]++
		return
	}
	c.failed++
	c.skipped["error"]++
}

var errTranscriptInvalid = errors.New("transcript would not stay valid")

func transcriptFile(path string, m *secretredact.Matcher, fix bool) (int, error) {
	if !fix || filepath.Ext(path) == ".json" {
		b, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		hits := m.Mask(b)
		if !fix {
			return hits, nil
		}
		if hits > 0 && !json.Valid(b) {
			return 0, errTranscriptInvalid
		}
	}
	return secretredact.File(path, m, transcriptIdle)
}
