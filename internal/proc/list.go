package proc

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DispatchIDEnv names the dispatch record id every dispatch child carries in its env, so a sweep can tell it apart.
const DispatchIDEnv = "HARNESS_DISPATCH_ID"

// Info is one row of the process table: identity, ancestry, start, executable and the value of the marker env var.
type Info struct {
	PID, PPID, PGID int
	Start           time.Time
	Exe             string // base name of the executable (ps comm, or /proc/<pid>/exe on Linux)
	Mark            string // the marker env var's value; "" when unset or the env is unreadable (not our process)
}

// lstartFields is how many fields ps lstart prints ("Wed Nov 11 11:17:11 2026").
const lstartFields = 5

// parseList parses `ps -axeww -o pid=,ppid=,pgid=,lstart=,command=` (BSD and GNU alike; `e` appends the environment
// to command); rows that do not parse are skipped. comms maps pid to its executable path (a path may hold a space,
// which command cannot show); a pid missing there falls back to command's first word. The environment is mark-only.
func parseList(out string, comms map[int]string, mark string, loc *time.Location) []Info {
	var list []Info
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3+lstartFields+1 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		pgid, err3 := strconv.Atoi(f[2])
		start, err4 := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(f[3:3+lstartFields], " "), loc)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		exe := comms[pid]
		if exe == "" {
			exe = f[3+lstartFields]
		}
		info := Info{PID: pid, PPID: ppid, PGID: pgid, Start: start, Exe: filepath.Base(exe)}
		for _, tok := range f[3+lstartFields+1:] {
			if v, ok := strings.CutPrefix(tok, mark+"="); ok {
				info.Mark = v
			}
		}
		list = append(list, info)
	}
	return list
}

// parseComms parses `ps -axo pid=,comm=` into pid -> executable path; the rest of a row after the pid may hold spaces.
func parseComms(out string) map[int]string {
	comms := map[int]string{}
	for line := range strings.SplitSeq(out, "\n") {
		pidStr, comm, ok := strings.Cut(strings.TrimSpace(line), " ")
		pid, err := strconv.Atoi(pidStr)
		if ok && err == nil {
			comms[pid] = strings.TrimSpace(comm)
		}
	}
	return comms
}
