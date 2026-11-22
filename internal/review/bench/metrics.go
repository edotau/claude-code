package bench

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/review/source"
)

const (
	bytesPerMB   = 1 << 20
	kbPerMB      = 1024
	probeTimeout = 10 * time.Second
)

// Env is the machine and build context of a run; a field that could not be collected stays empty.
type Env struct {
	Commit     string  `json:"commit"`
	Dirty      bool    `json:"dirty"`
	Go         string  `json:"go"`
	GOOS       string  `json:"goos"`
	GOARCH     string  `json:"goarch"`
	Machine    string  `json:"machine"`
	CPUs       int     `json:"cpus"`
	GOMAXPROCS int     `json:"gomaxprocs"`
	MemTotalMB int64   `json:"mem_total_mb,omitempty"`
	MemAvailMB int64   `json:"mem_avail_mb,omitempty"`
	Power      string  `json:"power,omitempty"`
	Thermal    string  `json:"thermal,omitempty"`
	Load1      float64 `json:"load1"`
}

// procUsage is the go test process's rusage; zero means not collected (Windows).
type procUsage struct {
	MaxRSSKB    int64 `json:"max_rss_kb,omitempty"`
	MinorFaults int64 `json:"minor_faults,omitempty"`
	MajorFaults int64 `json:"major_faults,omitempty"`
	VolCtx      int64 `json:"vol_ctx_switches,omitempty"`
	InvolCtx    int64 `json:"invol_ctx_switches,omitempty"`
}

// RunStats is what one package's go test process cost and the load around it.
type RunStats struct {
	WallSec    float64 `json:"wall_sec"`
	UserSec    float64 `json:"user_sec"`
	SysSec     float64 `json:"sys_sec"`
	LoadBefore float64 `json:"load_before"`
	LoadAfter  float64 `json:"load_after"`
	procUsage
}

func collectEnv(top string) Env {
	sha, _ := source.GitInherit(top, "rev-parse", "--short", "HEAD")
	dirty, _ := source.GitInherit(top, "status", "--porcelain")
	gov, _ := out("go", "version")
	total, avail := memInfo()
	e := Env{
		Commit: sha, Dirty: dirty != "", Go: strings.TrimSpace(string(gov)), GOOS: runtime.GOOS,
		GOARCH: runtime.GOARCH, Machine: machineName(), CPUs: runtime.NumCPU(), GOMAXPROCS: runtime.GOMAXPROCS(0),
		MemTotalMB: total, MemAvailMB: avail, Load1: loadAverage(),
	}
	if runtime.GOOS == "darwin" {
		e.Power, e.Thermal = powerSource(), thermalHint()
	}
	return e
}

func (e Env) lines() string {
	return fmt.Sprintf("commit: %s\ndirty: %t\ngo: %s\ngoos: %s\ngoarch: %s\nmachine: %s\ncpus: %d\ngomaxprocs: %d\n"+
		"mem-total-mb: %d\nmem-avail-mb: %d\npower: %s\nthermal: %s\n",
		e.Commit, e.Dirty, e.Go, e.GOOS, e.GOARCH, e.Machine, e.CPUs, e.GOMAXPROCS,
		e.MemTotalMB, e.MemAvailMB, e.Power, e.Thermal)
}

func (s RunStats) lines() string {
	return fmt.Sprintf("wall-sec: %.2f\nuser-sec: %.2f\nsys-sec: %.2f\nload-before: %.2f\nload-after: %.2f\n"+
		"max-rss-kb: %d\nminor-faults: %d\nmajor-faults: %d\nvol-ctx-switches: %d\ninvol-ctx-switches: %d\n",
		s.WallSec, s.UserSec, s.SysSec, s.LoadBefore, s.LoadAfter,
		s.MaxRSSKB, s.MinorFaults, s.MajorFaults, s.VolCtx, s.InvolCtx)
}

func statsOf(cmd *exec.Cmd, wall time.Duration, before float64) RunStats {
	s := RunStats{WallSec: round(wall.Seconds()), LoadBefore: before, LoadAfter: loadAverage()}
	if ps := cmd.ProcessState; ps != nil {
		s.UserSec, s.SysSec = round(ps.UserTime().Seconds()), round(ps.SystemTime().Seconds())
		s.procUsage = usageOf(ps)
	}
	return s
}

// parseLoad reads the 1-minute load from /proc/loadavg or sysctl's "{ 1.2 1.1 1.0 }" form.
func parseLoad(s string) float64 {
	f := strings.Fields(strings.Trim(strings.TrimSpace(s), "{}"))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

func loadAverage() float64 {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		return parseLoad(string(b))
	}
	if b, err := out("sysctl", "-n", "vm.loadavg"); err == nil {
		return parseLoad(string(b))
	}
	return 0
}

func machineName() string {
	if b, err := out("sysctl", "-n", "hw.model"); err == nil {
		return strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/sys/devices/virtual/dmi/id/product_name"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return "unknown"
}

// memInfo returns total and available MB, 0 when the platform has no cheap source.
func memInfo() (total, avail int64) {
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		return parseMeminfo(string(b))
	}
	if runtime.GOOS != "darwin" {
		return 0, 0
	}
	if b, err := out("sysctl", "-n", "hw.memsize"); err == nil {
		n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		total = n / bytesPerMB
	}
	if b, err := out("vm_stat"); err == nil {
		avail = parseVMStat(string(b))
	}
	return total, avail
}

func parseMeminfo(s string) (total, avail int64) {
	for line := range strings.SplitSeq(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = kb / kbPerMB
		case "MemAvailable:":
			avail = kb / kbPerMB
		}
	}
	return total, avail
}

// parseVMStat sums free, inactive and speculative pages into MB.
func parseVMStat(s string) int64 {
	var pageSize, pages int64
	for line := range strings.SplitSeq(s, "\n") {
		if _, rest, ok := strings.Cut(line, "page size of "); ok {
			f := strings.Fields(rest)
			if len(f) > 0 {
				pageSize, _ = strconv.ParseInt(f[0], 10, 64)
			}
			continue
		}
		name, val, ok := strings.Cut(line, ":")
		if !ok || (name != "Pages free" && name != "Pages inactive" && name != "Pages speculative") {
			continue
		}
		n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(val), "."), 10, 64)
		pages += n
	}
	return pages * pageSize / bytesPerMB
}

func powerSource() string {
	b, err := out("pmset", "-g", "batt")
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(first)
}

func thermalHint() string {
	b, err := out("pmset", "-g", "therm")
	if err != nil {
		return ""
	}
	var parts []string
	for line := range strings.SplitSeq(string(b), "\n") {
		if l := strings.TrimPrefix(strings.TrimSpace(line), "Note: "); l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, "; ")
}

func out(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}
