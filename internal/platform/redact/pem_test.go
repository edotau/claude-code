package redact

import (
	"strings"
	"testing"
)

func swapTestMarkers(t *testing.T) {
	t.Cleanup(SwapPEMMarkers("<<OPEN ", "<<CLOSE ", "WIDGET"))
}

func TestAfterOrphanPEMDropsThroughTheEndLine(t *testing.T) {
	swapTestMarkers(t)
	body := "gibberish body line\n"
	if got := AfterOrphanPEM(body+body+"<<CLOSE WIDGET>>\nafter: ok\n", false); got != "\nafter: ok\n" {
		t.Errorf("orphan block = %q, want only the text after the END line", got)
	}
	if got := AfterOrphanPEM("<<OPEN WIDGET>>\n"+body+"<<CLOSE WIDGET>>\nx\n", false); !strings.HasPrefix(got, "<<OPEN") {
		t.Errorf("whole block = %q, want it untouched", got)
	}
	if got := AfterOrphanPEM(body, true); got != "" {
		t.Errorf("open block with no END = %q, want empty", got)
	}
}

func TestPEMOpenAfterDropTracksTheBlockAcrossACut(t *testing.T) {
	swapTestMarkers(t)
	b := []byte("<<OPEN WIDGET>>\n" + strings.Repeat("gibberish body line\n", 5))
	if !PEMOpenAfterDrop(b, 30, false) {
		t.Error("begin in the dropped front, no end: want open")
	}
	b = []byte("<<OPEN WIDGET>>\nbody\n<<CLOSE WIDGET>>\n" + strings.Repeat("later line\n", 5))
	if PEMOpenAfterDrop(b, 60, true) {
		t.Error("end in the dropped front: want closed")
	}
}
