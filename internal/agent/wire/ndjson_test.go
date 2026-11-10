package wire

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

type testEvent struct {
	N int `json:"n"`
}

// recordUntil2 records each event's N and stops at N == 2.
func recordUntil2(got *[]int) func(testEvent, *TransportTracker) (bool, error) {
	return func(ev testEvent, _ *TransportTracker) (bool, error) {
		*got = append(*got, ev.N)
		return ev.N == 2, nil
	}
}

// TestDecodeEventsTickAndStop: tick times each event from Start, skipped lines are not events, stop keeps the rest.
func TestDecodeEventsTickAndStop(t *testing.T) {
	now := time.Unix(0, 0)
	orig := Clock
	Clock = func() time.Time { now = now.Add(time.Second); return now }
	t.Cleanup(func() { Clock = orig })

	var got []int
	br := bufio.NewReader(strings.NewReader("{\"n\":1}\n\nnot json\n{\"n\":2}\n{\"n\":3}\n"))
	tr, err := DecodeEvents(br, nil, "decode test", true, nil, recordUntil2(&got))
	if err != nil || len(got) != 2 || tr == nil || tr.FirstEventMs != time.Second.Milliseconds() {
		t.Fatalf("got %v, %+v, %v; want events 1,2 timed from Start and a stop at 2", got, tr, err)
	}
	if rest, _ := br.ReadString('\n'); rest != "{\"n\":3}\n" {
		t.Errorf("after stop the reader holds %q, want the next line kept", rest)
	}
}

// TestDecodeEventsBadLine: an undecodable line runs onBad and aborts with the label-wrapped error, untimed.
func TestDecodeEventsBadLine(t *testing.T) {
	var got []int
	var bad error
	tr, err := DecodeEvents(bufio.NewReader(strings.NewReader(`{"n":"x"}`)), nil, "decode test", false,
		func(e error) { bad = e }, recordUntil2(&got))
	if err == nil || bad != err || !strings.HasPrefix(err.Error(), "decode test: ") || tr != nil {
		t.Errorf("bad line = %v (onBad %v, transport %+v), want a label-wrapped abort seen by onBad", err, bad, tr)
	}
}
