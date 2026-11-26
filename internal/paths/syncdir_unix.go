//go:build unix

package paths

import (
	"errors"
	"os"
	"syscall"
)

// dirSync fsyncs a directory with a plain fsync(2); (*os.File).Sync on darwin is F_FULLFSYNC and costs ~26 ms.
func dirSync(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(syscall.Fsync(int(d.Fd())), d.Close())
}
