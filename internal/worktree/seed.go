package worktree

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func seedWorktree(main, dest string, log io.Writer) {
	for _, name := range []string{".venv", ".env"} {
		if err := seedPath(main, dest, name); err != nil {
			fmt.Fprintf(log, "worktree-create: warning: seed %s: %v\n", name, err)
		}
	}
}

func seedPath(main, dest, name string) error {
	source, err := filepath.Abs(filepath.Join(main, name))
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	target := filepath.Join(dest, name)
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		return err
	}
	if ignored, err := seedIgnored(dest, name); err != nil || !ignored {
		return err
	}
	if name == ".venv" {
		return os.Symlink(source, target)
	}
	return copyEnvFile(source, target, info.Mode().Perm())
}

// seedIgnored reports whether name is git-ignored in dest; the dest branch's rules govern what may be seeded.
func seedIgnored(dest, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	probe := name
	if name == ".venv" {
		probe += "/" // a `.venv/` rule only matches paths git treats as directories
	}
	if err := gitCmd(ctx, "-C", dest, "check-ignore", "-q", probe).Run(); err != nil {
		if code, isExit := asExitErr(err); isExit && code == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git check-ignore %s: %w", name, err)
	}
	return true, nil
}

// copyEnvFile copies source to a new target with perm, removing a partial target on failure.
func copyEnvFile(source, target string, perm os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if copyErr == nil {
		copyErr = out.Chmod(perm)
	}
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(target)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	return nil
}
