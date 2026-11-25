//go:build !unix

package paths

import "os"

// ReadMapped reads path whole (no mapping on this platform); release is a no-op, so data stays valid.
func ReadMapped(path string) (data []byte, release func(), err error) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return nil, noRelease, err
	}
	return b, noRelease, nil
}

func noRelease() {}
