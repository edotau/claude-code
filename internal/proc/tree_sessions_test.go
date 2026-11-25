package proc

import (
	"slices"
	"testing"
)

func TestTreeCancelSparesDetachedSessions(t *testing.T) {
	const root = 10
	sids := map[int]int{
		20: 20, // detached leader
		21: 20, // its member
		30: 10, // ordinary descendant
		40: 40, // unmarked setsid leader (a tool's own): killed
		41: 40, // its member
		31: -1, // unreadable: kept
		10: 10, // root as leader: kept
	}
	sid := func(p int) int { return sids[p] }
	marked := func(p int) bool { return p != 40 }
	tests := []struct {
		name string
		kids []int
		want []int
	}{
		{"leader spared", []int{20, 30}, []int{30}},
		{"member spared", []int{20, 21, 30}, []int{30}},
		{"unmarked leader killed", []int{40, 41, 30}, []int{40, 41, 30}},
		{"ordinary kept", []int{30, 31}, []int{30, 31}},
		{"root as leader kept", []int{10, 30}, []int{10, 30}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := killable(tc.kids, root, sid, marked); !slices.Equal(got, tc.want) {
				t.Fatalf("killable(%v) = %v, want %v", tc.kids, got, tc.want)
			}
		})
	}
}
