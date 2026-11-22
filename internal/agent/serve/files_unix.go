//go:build unix

package serve

import (
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func openServeFile(name string) (*os.File, error) {
	fd, err := unix.Open(Dir(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), Dir())
	defer dir.Close()
	if err := checkServeDir(fd); err != nil {
		return nil, err
	}
	n, err := unix.Openat(fd, name, unix.O_CREAT|unix.O_WRONLY|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
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
	mask := unix.Umask(0o177)
	defer unix.Umask(mask)
	return net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
}
