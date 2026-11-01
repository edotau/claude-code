package inflight

import "testing"

// Cap math: Total = min(Base × max(len(available),1), 4 × cpus); For uses the override else Base.
func TestComputeLimits(t *testing.T) {
	cases := []struct {
		name      string
		available int
		cpus      int
		want      int
	}{
		{"accounts bind before cpus", 7, 16, 64},
		{"cpus bind", 1, 2, 8},
		{"no accounts still allows one base", 0, 16, 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lim := ComputeLimits(make([]string, tc.available), nil, tc.cpus)
			if lim.Total != tc.want {
				t.Errorf("ComputeLimits(%d accounts, %d cpus).Total = %d, want %d",
					tc.available, tc.cpus, lim.Total, tc.want)
			}
			if lim.Base != Base {
				t.Errorf("ComputeLimits().Base = %d, want %d", lim.Base, Base)
			}
		})
	}
	lim := ComputeLimits([]string{"a"}, map[string]int{"a": 3}, 16)
	if got := lim.For("a"); got != 3 {
		t.Errorf("For with override = %d, want 3", got)
	}
	if got := lim.For("b"); got != Base {
		t.Errorf("For without override = %d, want Base %d", got, Base)
	}
}
