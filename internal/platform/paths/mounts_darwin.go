package paths

import "golang.org/x/sys/unix"

func readMounts() []Mount {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil || n <= 0 {
		return nil
	}
	buf := make([]unix.Statfs_t, n+8)
	n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return nil
	}
	var out []Mount
	for i := range buf[:min(n, len(buf))] {
		s := &buf[i]
		point, src := unix.ByteSliceToString(s.Mntonname[:]), unix.ByteSliceToString(s.Mntfromname[:])
		m, ok := classify(point, src, unix.ByteSliceToString(s.Fstypename[:]))
		if !ok {
			m = Mount{Point: point, Kind: kindOpaque}
		}
		out = append(out, m)
	}
	return out
}
