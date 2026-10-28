package proc

import (
	"os"
	"runtime"
	"testing"
)

func TestStartTimeOfSelfIsStable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux /proc identity")
	}
	a, err := StartTime(os.Getpid())
	b, _ := StartTime(os.Getpid())
	if err != nil || a == "" || a != b {
		t.Fatalf("StartTime = %q, %q, %v; want non-empty and stable", a, b, err)
	}
	if _, err := StartTime(1 << 30); err == nil {
		t.Fatal("a missing pid must error")
	}
}
