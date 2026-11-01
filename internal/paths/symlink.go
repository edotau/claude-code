package paths

import (
	"errors"
	"fmt"
	"os"
)

// SwapSymlink points link at target via a temp link and rename, so link is never absent or half-made.
func SwapSymlink(target, link string) error {
	tmp := fmt.Sprintf("%s.tmp%d", link, os.Getpid())
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return nil
}
