package agent

import (
	"os"
	"runtime"
	"testing"
)

func TestStampProcessGroupSelf(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no process groups")
	}
	r := Record{PID: os.Getpid()}
	r.stampProcessGroup()
	if r.PPID != os.Getppid() || r.PGID <= 0 {
		t.Fatalf("PPID = %d, PGID = %d", r.PPID, r.PGID)
	}
	none := Record{}
	none.stampProcessGroup()
	if none.PGID != 0 {
		t.Fatalf("PGID = %d for pid 0", none.PGID)
	}
}
