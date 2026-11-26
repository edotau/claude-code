package proc

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"
)

// profileWindow bounds the CPU profile taken after each signal.
const profileWindow = 10 * time.Second

// profileSignal is SIGUSR1 per GOOS/GOARCH (0: none, so the feature is a no-op there).
func profileSignal() syscall.Signal {
	switch {
	case runtime.GOOS == "linux" && strings.HasPrefix(runtime.GOARCH, "mips"):
		return 16
	case runtime.GOOS == "linux" || runtime.GOOS == "android":
		return 10
	case runtime.GOOS == "darwin" || strings.HasSuffix(runtime.GOOS, "bsd") || runtime.GOOS == "dragonfly":
		return 30
	}
	return 0
}

// ProfileOnSignal writes heap and CPU profiles into dir() on each SIGUSR1 and returns a stop that ends a capture early.
func ProfileOnSignal(dir func() string) (stop func()) {
	sig := profileSignal()
	if sig == 0 {
		return func() {}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sig)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-ch:
				if err := writeProfiles(dir(), profileWindow, done); err != nil {
					fmt.Fprintln(os.Stderr, "profile:", err)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		<-finished // an in-flight capture flushes its files first
	}
}

func createExcl(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func writeProfiles(dir string, window time.Duration, done <-chan struct{}) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	base := filepath.Join(dir, fmt.Sprintf("%d-%s", os.Getpid(), time.Now().Format("20060102T150405.000")))
	heap, err := createExcl(base + ".heap.pprof")
	if err != nil {
		return err
	}
	runtime.GC() // current live heap, not garbage awaiting collection
	werr := pprof.Lookup("heap").WriteTo(heap, 0)
	if cerr := heap.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	cpuPath := base + ".cpu.pprof"
	cpu, err := createExcl(cpuPath)
	if err != nil {
		return err
	}
	if err := pprof.StartCPUProfile(cpu); err != nil {
		_ = cpu.Close()
		_ = os.Remove(cpuPath) // another pprof user owns the profiler: leave no empty file
		return err
	}
	select {
	case <-time.After(window):
	case <-done:
	}
	pprof.StopCPUProfile()
	return cpu.Close()
}
