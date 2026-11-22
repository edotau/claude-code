package proc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// ErrLoadUnknown means this platform cannot report a one-minute load average.
var ErrLoadUnknown = errors.New("one-minute load average unavailable")

// QuietMachine checks one-minute load against half the CPU count and active dispatches.
func QuietMachine(ctx context.Context, running int) (bool, float64, error) {
	if running < 0 {
		return false, 0, fmt.Errorf("negative running dispatch count: %d", running)
	}
	var raw []byte
	var err error
	switch runtime.GOOS {
	case "linux":
		raw, err = os.ReadFile("/proc/loadavg")
	case "darwin":
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		raw, err = exec.CommandContext(probeCtx, "sysctl", "-n", "vm.loadavg").Output()
	default:
		return false, 0, ErrLoadUnknown
	}
	if err != nil {
		return false, 0, fmt.Errorf("read one-minute load average: %w", err)
	}
	var load float64
	_, err = fmt.Fscan(strings.NewReader(strings.Trim(strings.TrimSpace(string(raw)), "{}")), &load)
	if err != nil || math.IsNaN(load) || math.IsInf(load, 0) || load < 0 {
		return false, 0, fmt.Errorf("invalid one-minute load average: %q", raw)
	}
	return running == 0 && load <= float64(runtime.NumCPU())/2, load, nil
}
