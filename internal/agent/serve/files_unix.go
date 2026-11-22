//go:build unix

package serve

import (
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func openServeFile(name string, flags int) (*os.File, error) {
	fd, err := unix.Open(Dir(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), Dir())
	defer dir.Close()
	if err := checkServeDir(fd); err != nil {
		return nil, err
	}
	n, err := unix.Openat(fd, name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(n), name)
	var st unix.Stat_t
	if err := unix.Fstat(n, &st); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 || st.Mode&0o077 != 0 {
		return nil, errors.Join(fmt.Errorf("agents serve: unsafe opened file %s", name), f.Close())
	}
	if err := unix.SetNonblock(n, false); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func checkServeDir(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != uint32(os.Getuid()) || st.Mode&0o077 != 0 {
		return errors.New("agents serve: unsafe opened directory")
	}
	return nil
}

func listenPrivate(path string) (*net.UnixListener, error) {
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, errors.Join(err, ln.Close())
	}
	return ln, nil
}
