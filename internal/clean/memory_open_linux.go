package clean

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/memory"
)

// openMemoryFiles lists state files held open by same-user processes, lock descriptors included.
func openMemoryFiles() (map[string]bool, bool) {
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
	if err != nil {
		return true // an exited or unreadable process cannot be inspected; the session registry still guards
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
