//go:build !unix

package serve

import (
	"net"
	"os"
)

func openServeFile(string) (*os.File, error) { return nil, ErrUnsupported }

func listenPrivate(string) (*net.UnixListener, error) { return nil, ErrUnsupported }
