package proc

import (
	"bytes"
	"os"
	"strconv"
)

// tagged returns the pids whose initial env holds tag, from /proc/<pid>/environ; another user's process is unreadable.
func tagged(tag string) ([]int, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	buf := make([]byte, 1<<16) // one buffer for every read: os.ReadFile regrows from 512 B per process
	var pids []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err == nil && pid > 1 && pid != self && envHolds(readEnviron(e.Name(), buf), tag) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func hasTag(pid int, tag string) bool {
	return envHolds(readEnviron(strconv.Itoa(pid), make([]byte, 1<<16)), tag)
}

// envHolds reports a NUL-separated env block with an entry named tag.
func envHolds(env []byte, tag string) bool {
	want := []byte("\x00" + tag + "=")
	return bytes.HasPrefix(env, want[1:]) || bytes.Contains(env, want)
}

// readEnviron reads /proc/<pid>/environ into buf, growing it for a larger env; nil when unreadable.
func readEnviron(pid string, buf []byte) []byte {
	f, err := os.Open("/proc/" + pid + "/environ")
	if err != nil {
		return nil
	}
	defer f.Close()
	n := 0
	for {
		m, err := f.Read(buf[n:])
		n += m
		if err != nil || m == 0 {
			return buf[:n]
		}
		if n == len(buf) {
			buf = append(buf, make([]byte, len(buf))...)
		}
	}
}
