package proxy

import (
	"slices"
	"testing"
)

func TestParseListenerInput(t *testing.T) {
	if got := parseLsofPIDs("p123\nn127.0.0.1:18765\np456\nn[::1]:18765\np123\nn*:18765\n"); !slices.Equal(got, []int{123, 456}) {
		t.Errorf("parseLsofPIDs = %v, want [123 456]", got)
	}
	tcp := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:494D 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 777 1 0\n" +
		"   1: 0100007F:494D 0100007F:9999 01 00000000:00000000 00:00000000 00000000  1000        0 888 1 0\n" +
		"   2: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 999 1 0\n"
	if got := parseProcNetInodes(tcp, 18765); !slices.Equal(got, []string{"777"}) {
		t.Errorf("parseProcNetInodes = %v, want [777]", got)
	}
}
