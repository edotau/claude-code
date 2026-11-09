package setup

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// Backup copies each of relPaths (dir-relative, slash-separated) into paths.BackupDir(label, now),
// preserving the file mode.
func Backup(dir string, relPaths []string, label string, now time.Time) (string, error) {
	dest := paths.BackupDir(label, now)
	for _, rel := range relPaths {
		if err := copyFile(filepath.Join(dir, rel), filepath.Join(dest, rel)); err != nil {
			return dest, err
		}
	}
	return dest, nil
}

func copyFile(src, dst string) error {
	if li, err := os.Lstat(src); err == nil && li.Mode()&os.ModeSymlink != 0 {
		return copyLink(src, dst) // a symlink (even a dangling one) is backed up as a link, never followed
	}
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return copyTree(src, dst)
	}
	if !fi.Mode().IsRegular() {
		return nil // FIFOs, sockets and devices are skipped: reading them can block forever
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return paths.AtomicWrite(dst, data, fi.Mode())
}

// copyTree backs up a directory's files and links under dst; symlinks inside are copied as links, never followed.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		return copyFile(p, filepath.Join(dst, rel))
	})
}

func copyLink(src, dst string) error {
	target, err := os.Readlink(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(target, dst)
}

// Report prints one line per clash: path, same|differs, and where it lands under backupDir.
func Report(w io.Writer, clashes []Clash, backupDir string) {
	fmt.Fprintf(w, "%-50s %-8s %s\n", "PATH", "STATE", "BACKUP")
	for _, c := range clashes {
		state := "same"
		if c.Differs {
			state = "differs"
		}
		fmt.Fprintf(w, "%-50s %-8s %s\n", c.Path, state, filepath.Join(backupDir, c.Path))
	}
}
