package rtkwrap

import (
	"bytes"
	"encoding/binary"
	"strings"

	"golang.org/x/sys/unix"
)

// argvOf is pid's argv from procargs2 (argc, exec path, NUL padding, argv...); nil when unreadable.
func argvOf(pid int) []string {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) <= 4 {
		return nil
	}
	argc := int(binary.NativeEndian.Uint32(raw))
	i := bytes.IndexByte(raw[4:], 0)
	if argc <= 0 || i < 0 {
		return nil
	}
	parts := strings.SplitN(strings.TrimLeft(string(raw[4+i:]), "\x00"), "\x00", argc+1)
	if len(parts) < argc {
		return nil
	}
	return parts[:argc]
}
