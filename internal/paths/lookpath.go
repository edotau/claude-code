package paths

import (
	"errors"
	"os"
	"path/filepath"
)

// LookPathReal finds name on PATH skipping BinDir and any symlink to the running executable, so a
// `claude` shim never re-execs itself.
func LookPathReal(name string) (string, error) {
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	shimDir, _ := filepath.Abs(BinDir())
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if abs, _ := filepath.Abs(dir); abs == shimDir || dir == "" {
			continue
		}
		cand := filepath.Join(dir, name)
		fi, err := os.Stat(cand)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		if real, _ := filepath.EvalSymlinks(cand); self != "" && real == self {
			continue
		}
		return cand, nil
	}
	return "", errors.New(name + ": not found on PATH (outside " + shimDir + ")")
}
