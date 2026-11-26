package paths

import (
	"fmt"
	"os"
)

func noRelease() {}

// statRegular stats path and refuses anything that is not a regular file, before any open can block.
func statRegular(path string) (os.FileInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("map %s: not a regular file", path)
	}
	return fi, nil
}
