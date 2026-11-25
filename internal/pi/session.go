package pi

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// sessionDir is where Pi saves the sessions that --session resumes.
func sessionDir() string { return filepath.Join(paths.StateDir(), "pi", "sessions") }

// ensureSessionDir makes the session dir owner-only: its files hold prompts and tool output.
func ensureSessionDir() error {
	dir := sessionDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// sessionIDOf is the id in a "<timestamp>_<id>.jsonl" name; the timestamp holds no underscore.
func sessionIDOf(name string) (string, bool) {
	base, ok := strings.CutSuffix(name, ".jsonl")
	if !ok {
		return "", false
	}
	_, id, ok := strings.Cut(base, "_")
	return id, ok
}

// sessionFile is the one saved session file for id under dir, "" when none; two files for one id is an error.
func sessionFile(dir, id string) (string, error) {
	if id == "" {
		return "", nil
	}
	var found []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if got, ok := sessionIDOf(d.Name()); ok && got == id {
			found = append(found, p)
		}
		return nil
	})
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("pi session %s is saved twice (%s and %s); remove one", id, found[0], found[1])
}

// sessionCwd is the directory a saved session was started in, "" when no file holds id.
func sessionCwd(dir, id string) (string, error) {
	file, err := sessionFile(dir, id)
	if file == "" || err != nil {
		return "", err
	}
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var hdr struct{ Cwd string }
	if err := json.NewDecoder(f).Decode(&hdr); err != nil {
		return "", fmt.Errorf("pi session %s: unreadable header in %s: %w", id, file, err)
	}
	if st, err := os.Stat(hdr.Cwd); err != nil || !st.IsDir() {
		return "", fmt.Errorf("pi session %s ran in %q, which is no longer a directory; start a new run with --retry", id, hdr.Cwd)
	}
	return hdr.Cwd, nil
}

// resumeDir is where a resume must run: Pi prompts to fork a session from any other directory.
func resumeDir(dir, id, workDir string) (string, error) {
	cwd, err := sessionCwd(dir, id)
	if cwd == "" || err != nil {
		return workDir, err
	}
	if workDir != "" && !sameDir(workDir, cwd) {
		return "", fmt.Errorf("pi session %s ran in %s but the workdir is %s; drop the workdir flag or start a new run", id, cwd, workDir)
	}
	return cwd, nil
}

func sameDir(a, b string) bool {
	for _, p := range []*string{&a, &b} {
		if r, err := filepath.EvalSymlinks(*p); err == nil {
			*p = r
		}
		*p = filepath.Clean(*p)
	}
	return a == b
}
