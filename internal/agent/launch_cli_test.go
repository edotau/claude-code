package agent

import "testing"

func TestInteractiveVendorArgs(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, true},
		{[]string{"--version"}, false},
		{[]string{"-v"}, false},
		{[]string{"-V"}, false},
		{[]string{"--help"}, false},
		{[]string{"-h"}, false},
		{[]string{"run", "x"}, false},
		{[]string{"--model", "m"}, true},
		{[]string{"chat", "--version"}, false},
	}
	for _, tc := range cases {
		if got := interactiveArgs(tc.args); got != tc.want {
			t.Errorf("interactiveVendorArgs(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
