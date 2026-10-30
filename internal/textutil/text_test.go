package textutil

import "testing"

func TestPlural(t *testing.T) {
	if Plural(1) != "" || Plural(0) != "s" || Plural(2) != "s" || Plural(-1) != "s" {
		t.Errorf("Plural: 1=%q 0=%q 2=%q -1=%q", Plural(1), Plural(0), Plural(2), Plural(-1))
	}
	if got := PluralWord(1, "y", "ies"); got != "y" {
		t.Errorf("PluralWord(1, y, ies) = %q, want y", got)
	}
	if got := PluralWord(2, "y", "ies"); got != "ies" {
		t.Errorf("PluralWord(2, y, ies) = %q, want ies", got)
	}
}

func TestIsDigits(t *testing.T) {
	for s, want := range map[string]bool{"": false, "0": true, "123": true, "12a": false, "-1": false, "１": false} {
		if got := IsDigits(s); got != want {
			t.Errorf("IsDigits(%q) = %v, want %v", s, got, want)
		}
	}
}

// TestHumanBytes: binary units (KiB spelling, base 1024), 1 decimal past B, up to PiB.
func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1024*1024 + 512*1024, "1.5 MiB"},
		{1024 * 1024 * 1024 * 1024 * 1024, "1.0 PiB"},
	} {
		if got := HumanBytes(tc.n); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
