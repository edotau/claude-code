//go:build unix

package paths

import (
	"fmt"
	"os"
	"sync"
	"syscall"
)

// ReadMapped maps path read-only; data dies at release, and in-place truncation SIGBUSes: writers must rename.
func ReadMapped(path string) (data []byte, release func(), err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, noRelease, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, noRelease, err
	}
	if !fi.Mode().IsRegular() {
		return nil, noRelease, fmt.Errorf("map %s: not a regular file", path)
	}
	size := fi.Size()
	if size == 0 {
		return nil, noRelease, nil
	}
	if int64(int(size)) != size {
		return readFallback(path)
	}
	m, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return readFallback(path)
	}
	var once sync.Once
	return m, func() { once.Do(func() { _ = syscall.Munmap(m) }) }, nil
}

func noRelease() {}

func readFallback(path string) ([]byte, func(), error) {
	b, err := os.ReadFile(path)
	return b, noRelease, err
}
