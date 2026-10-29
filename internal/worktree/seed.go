package worktree

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	if gitCmd(context.Background(), "-C", main, "check-ignore", "-q", name).Run() != nil {
		return nil
	}
	if name == ".venv" {
		return os.Symlink(source, target)
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	if copyErr == nil {
		copyErr = out.Chmod(info.Mode().Perm())
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
