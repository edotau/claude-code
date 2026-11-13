package worktree

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
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
		if err := os.Symlink(source, target); err != nil {
			return err
		}
		return excludeSymlink(dest, name)
	}
	return copyEnvFile(source, target, info.Mode().Perm())
}

// seedProbeTimeout bounds the git check-ignore that gates seeding a path into a new worktree.
const seedProbeTimeout = 10 * time.Second

// seedIgnored reports whether name is git-ignored in dest; the dest branch's rules govern what may be seeded.
func seedIgnored(dest, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), seedProbeTimeout)
	defer cancel()
	probe := name
	if name == ".venv" {
		probe += "/" // a `.venv/` rule only matches paths git treats as directories
	}
	return checkIgnored(ctx, dest, probe)
}

// checkIgnored runs git check-ignore for probe in dest: exit 0 is ignored, the clean answer exit 1 is not.
func checkIgnored(ctx context.Context, dest, probe string) (bool, error) {
	if err := gitCmd(ctx, "-C", dest, "check-ignore", "-q", probe).Run(); err != nil {
		if code, isExit := asExitErr(err); isExit && code == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git check-ignore %s: %w", probe, err)
	}
	return true, nil
}

// excludeSymlink adds /name to the repo's info/exclude when a directory-only rule leaves the seeded symlink untracked.
func excludeSymlink(dest, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), seedProbeTimeout)
	defer cancel()
	if ignored, err := checkIgnored(ctx, dest, name); err != nil || ignored {
		return err
	}
	out, err := gitCmd(ctx, "-C", dest, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude").Output()
	if err != nil {
		return fmt.Errorf("git rev-parse --git-path info/exclude: %w", err)
	}
	file, line := strings.TrimSpace(string(out)), "/"+name+"\n"
	data, _ := os.ReadFile(file) // a missing exclude file is created below
	if strings.Contains("\n"+string(data), "\n"+line) {
		return nil
	}
	return paths.AppendLineRepair(file, []byte(line)) // starts on its own row when the file lacks a final newline
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
	err = cmp.Or(copyErr, out.Close())
	if err != nil {
		_ = os.Remove(target)
	}
	return err
}
