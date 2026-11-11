package setup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/memory"
)

// scanOpenMemoryFiles lists state files held open by same-user processes, lock descriptors included.
func scanOpenMemoryFiles() (map[string]bool, bool) {
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	opened := map[string]bool{}
	for _, process := range processes {
		if _, err := strconv.Atoi(process.Name()); err != nil {
			continue
		}
		dir := filepath.Join("/proc", process.Name())
		fi, err := os.Stat(dir)
		if err != nil {
			continue
		}
		stat, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) {
			continue
		}
		if !memoryProcessFiles(dir, opened) {
			return nil, false
		}
	}
	return opened, true
}

// memoryProcessFiles records open state files without opening or changing runtime state.
func memoryProcessFiles(dir string, opened map[string]bool) bool {
	fds, err := os.ReadDir(filepath.Join(dir, "fd"))
	if errors.Is(err, fs.ErrNotExist) {
		return true // the process exited between listing and reading: it holds no file open
	}
	if err != nil {
		return false // unreadable fd table: open files are unknown, so the caller keeps state
	}
	root := filepath.Clean(memory.StateDir()) + string(filepath.Separator)
	for _, fd := range fds {
		p, err := os.Readlink(filepath.Join(dir, "fd", fd.Name()))
		if err != nil {
			continue
		}
		if strings.HasPrefix(p, root) {
			opened[filepath.Clean(p)] = true
		}
	}
	return true
}
