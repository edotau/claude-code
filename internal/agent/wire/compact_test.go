package wire

import "testing"

// Given counts around the k/M boundary, Then a count that would round to 1000.0k spells as millions.
func TestCompactCountRoundsUpIntoMillions(t *testing.T) {
	for n, want := range map[int]string{
		950: "950", 23_400: "23.4k", 999_949: "999.9k", 999_950: "1.00M", 999_999: "1.00M", 1_840_000: "1.84M",
	} {
		if got := compactCount(n); got != want {
			t.Errorf("compactCount(%d) = %q, want %q", n, got, want)
		}
	}
}
