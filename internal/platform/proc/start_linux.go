package proc

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// StartTime returns an opaque token identifying pid's start (stat field 22, clock ticks since boot).
func StartTime(pid int) (string, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNoProcess
	}
	if err != nil {
		return "", err
	}
	_, after, ok := strings.CutLast(string(raw), ") ")
	if !ok {
		return "", errors.New("proc: unparsable stat")
	}
	f := strings.Fields(after) // after comm: field 3 (state) is f[0], so field 22 is f[19]
	if len(f) < 20 {
		return "", errors.New("proc: short stat")
	}
	return f[19], nil
}
