package cli

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"testing"
)

func TestCaptureAnswerClosesPipe(t *testing.T) {
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("requires /proc/self/fd")
	}
	gc := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gc)
	want := strings.Repeat("review answer\n", 8192)
	for range 16 {
		got, code := captureAnswer(func() int {
			fmt.Fprint(os.Stdout, want)
			return 7
		})
		if got != want || code != 7 {
			t.Fatalf("capture: bytes=%d, code=%d; want bytes=%d, code=7", len(got), code, len(want))
		}
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) > len(before) {
		t.Fatalf("capture leaked descriptors: before=%d after=%d", len(before), len(after))
	}
}
