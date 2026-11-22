//go:build !windows

package bench

import (
	"os"
	"runtime"
	"syscall"
)

const bytesPerKB = 1024

func usageOf(ps *os.ProcessState) procUsage {
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return procUsage{}
	}
	rss := int64(ru.Maxrss)
	if runtime.GOOS == "darwin" {
		rss /= bytesPerKB // darwin reports bytes, the rest report KB
	}
	return procUsage{
		MaxRSSKB: rss, MinorFaults: int64(ru.Minflt), MajorFaults: int64(ru.Majflt),
		VolCtx: int64(ru.Nvcsw), InvolCtx: int64(ru.Nivcsw),
	}
}
