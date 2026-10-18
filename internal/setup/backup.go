package setup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// BackupPath is where Backup(dir, _, label, now) copies the clash set.
func BackupPath(dir, label string, now time.Time) string {
	return filepath.Join(dir, "backups", label+"-"+now.Format("20060102-150405"))
}

// Backup copies each of relPaths (dir-relative, slash-separated) into BackupPath(dir, label, now),
// preserving the file mode. A path under env.d/ is never copied, even if passed in — a secret stays
// where it is, same exclusion the `history` verb applies.
func Backup(dir string, relPaths []string, label string, now time.Time) (string, error) {
	dest := BackupPath(dir, label, now)
	for _, rel := range relPaths {
		if strings.HasPrefix(filepath.ToSlash(rel), "env.d/") {
			continue
		}
		if err := copyFile(filepath.Join(dir, rel), filepath.Join(dest, rel)); err != nil {
			return dest, err
		}
	}
	return dest, nil
}

func copyFile(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return paths.AtomicWrite(dst, data, fi.Mode())
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
