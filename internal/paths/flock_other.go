//go:build !unix && !windows

package paths

import (
	"errors"
	"fmt"
	"os"
)

var errNoFileLock = fmt.Errorf("file locking: %w", errors.ErrUnsupported)

func unlock(*os.File) error { return nil }

// tryLock fails closed: there is no lock primitive here, so no caller may believe it holds one.
func tryLock(*os.File) (bool, error) { return false, errNoFileLock }
