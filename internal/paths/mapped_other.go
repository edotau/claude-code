//go:build !unix

package paths

import "os"

// ReadMapped reads path whole (no mapping here); only regular files, and the data stays valid after release.
func ReadMapped(path string) (data []byte, release func(), err error) {
	if _, err := statRegular(path); err != nil {
		return nil, noRelease, err
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return nil, noRelease, err
	}
	return b, noRelease, nil
}
