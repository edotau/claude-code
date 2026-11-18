package hooks

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/settings"
)

// secretsJobFlag is the internal `hook SessionEnd` flag the detached masking pass runs under; no module declares it.
const secretsJobFlag = "--secrets-end-job"

const (
	maxSessionFiles = 5000
	maxIdleRounds   = 4
)

// secretsIdle is the quiet a file needs first: the transcript writer lags and /clear or resume may continue it.
var secretsIdle = 30 * time.Second

// secretsLockWait bounds how long a queued pass waits for the one running pass.
var secretsLockWait = 3 * time.Minute

var sleepFn = time.Sleep

// spawnSecretsJob is the detached-pass seam; tests replace it.
var spawnSecretsJob = func(args []string) bool {
	self, err := os.Executable()
	return err == nil && proc.SpawnDetached(self, args, "", nil)
}

// SecretsEnd (SessionEnd) starts a detached pass over the ended session's transcripts and returns at once.
func SecretsEnd(in *core.Input) core.Result {
	if in.TranscriptPath == "" {
		return core.Result{}
	}
	root, ok := projectsRoot()
	if !ok {
		return core.Result{}
	}
	path, ok := underProjects(root, in.TranscriptPath)
	if !ok {
		return core.Result{Stderr: "hook secrets-end: transcript is not a regular file under projects/; skipped"}
	}
	reason := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r == '_' {
			return r
		}
		return -1
	}, in.Nested("reason"))
	spawnSecretsJob([]string{"hook", settings.EventSessionEnd, secretsJobFlag, path, reason[:min(len(reason), 24)]})
	return core.Result{}
}

func projectsRoot() (string, bool) {
	root, err := filepath.EvalSymlinks(filepath.Join(paths.ConfigDir(), "projects"))
	return root, err == nil
}

// underProjects resolves p and returns it only when it is a regular file, not itself a link, inside root.
func underProjects(root, p string) (string, bool) {
	p = filepath.Clean(p)
	dir, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		return "", false
	}
	full := filepath.Join(dir, filepath.Base(p))
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if fi, err := os.Lstat(full); err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	return full, true
}

// sessionFiles is the transcript plus the files under <session>/subagents and <session>/tool-results.
func sessionFiles(root, transcript string) []string {
	files := []string{transcript}
	sessionDir := strings.TrimSuffix(transcript, filepath.Ext(transcript))
	_ = filepath.WalkDir(sessionDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || len(files) >= maxSessionFiles {
			return fs.SkipAll
		}
		rel, _ := filepath.Rel(sessionDir, p)
		if !d.Type().IsRegular() || !strings.HasPrefix(rel, "subagents"+string(filepath.Separator)) &&
			!strings.HasPrefix(rel, "tool-results"+string(filepath.Separator)) {
			return nil
		}
		if full, ok := underProjects(root, p); ok {
			files = append(files, full)
		}
		return nil
	})
	return files
}

// waitIdle sleeps until every file has been quiet for secretsIdle, a few rounds at most; redact.FileAt rechecks each.
func waitIdle(files []string) {
	for range maxIdleRounds {
		var newest time.Time
		for _, f := range files {
			if fi, err := os.Stat(f); err == nil && fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
		wait := secretsIdle - time.Since(newest)
		if wait <= 0 {
			return
		}
		sleepFn(wait)
	}
}

// runSecretsJob is the detached pass: wait for quiet, then mask under one machine-wide lock.
func runSecretsJob(event string, args []string) int {
	root, ok := projectsRoot()
	if event != settings.EventSessionEnd || len(args) != 2 || !ok {
		return 1
	}
	path, ok := underProjects(root, args[0])
	if !ok {
		return 1
	}
	dir, err := os.OpenRoot(root) // opened before the wait, so a later swap of any path component cannot move a write
	if err != nil {
		return 1
	}
	defer dir.Close()
	files := sessionFiles(root, path)
	waitIdle(files)
	var masked, hits, failed int
	_ = os.MkdirAll(filepath.Join(paths.StateDir(), "hooks"), 0o700)
	ran, _ := paths.TryFileLock(filepath.Join(paths.StateDir(), "hooks", "secrets-end.lock"), secretsLockWait, func() error {
		m := redact.Shared()
		for _, f := range files {
			rel, err := filepath.Rel(root, f)
			n := 0
			if err == nil {
				n, err = redact.FileAt(dir, rel, m, secretsIdle)
			}
			if err != nil {
				failed++
			}
			hits += n
			if n > 0 {
				masked++
			}
		}
		return nil
	})
	if !ran {
		failed = len(files)
	}
	if hits > 0 || failed > 0 {
		line := fmt.Sprintf("%s reason=%s file=%s files=%d masked=%d hits=%d failed=%d\n", time.Now().UTC().Format(time.RFC3339),
			args[1], filepath.Base(path), len(files), masked, hits, failed)
		_ = paths.AppendLine(filepath.Join(paths.StateDir(), "hooks", "secrets-end.log"), []byte(line))
	}
	return 0
}
