// Package setup implements the "adopt" and "nuke" setup modes shared by the `setup` CLI verb.
package setup

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Clash is a git-tracked path in the source repo that already exists at the target dir.
type Clash struct {
	Path    string      // repo-relative, slash-separated
	Size    int64       // the target's current size
	Mode    os.FileMode // the target's current mode
	Differs bool        // sha256 differs from the repo's version
}

// Clashes lists every git-tracked path in repo that already exists at dir. Untracked and ignored
// paths in dir are never clashes — they are the user's own content, never the repo's business.
func Clashes(repo, dir string) ([]Clash, error) {
	tracked, err := trackedFiles(repo)
	if err != nil {
		return nil, err
	}
	var out []Clash
	for _, rel := range tracked {
		target := filepath.Join(dir, rel)
		fi, err := os.Stat(target)
		if err != nil {
			continue // not present at dir: no clash
		}
		differs, err := filesDiffer(filepath.Join(repo, rel), target)
		if err != nil {
			return nil, err
		}
		out = append(out, Clash{Path: rel, Size: fi.Size(), Mode: fi.Mode(), Differs: differs})
	}
	return out, nil
}

// trackedFiles runs `git -C repo ls-files -z` and returns repo-relative, slash-separated paths.
func trackedFiles(repo string) ([]string, error) {
	out, err := exec.Command("git", "-C", repo, "ls-files", "-z").Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, p := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if p != "" {
			files = append(files, p)
		}
	}
	return files, nil
}

// filesDiffer sha256-compares two files.
func filesDiffer(a, b string) (bool, error) {
	ah, err := hashFile(a)
	if err != nil {
		return false, err
	}
	bh, err := hashFile(b)
	if err != nil {
		return false, err
	}
	return !bytes.Equal(ah, bh), nil
}

func hashFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
