//go:build unix

package paths

import (
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
)

// ReadMapped maps path read-only; only for files every writer replaces by rename, and data is invalid after release.
func ReadMapped(path string) (data []byte, release func(), err error) {
	pre, err := statRegular(path)
	if err != nil {
		return nil, noRelease, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, noRelease, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, noRelease, err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(pre, fi) {
		return nil, noRelease, fmt.Errorf("map %s: not a regular file or replaced while opening", path)
	}
	size := fi.Size()
	if size == 0 {
		return nil, noRelease, nil
	}
	if int64(int(size)) != size {
		return readAll(f)
	}
	m, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return readAll(f)
	}
	var once sync.Once
	return m, func() { once.Do(func() { _ = syscall.Munmap(m) }) }, nil
}

func readAll(f *os.File) ([]byte, func(), error) {
	b, err := io.ReadAll(f)
	return b, noRelease, err
}
