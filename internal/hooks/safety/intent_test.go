package safety

import "testing"

// Given xargs options with values, Then the command past them is judged: a nested sudo is seen, a plain one is not.
func TestSudoRunsPastXargsOptions(t *testing.T) {
	for cmd, want := range map[string]bool{
		"xargs -n 1 /usr/bin/xsudo id": false, "xargs -n 1 /usr/bin/sudo id": true, "xargs -I {} /usr/bin/sudo id {}": true,
		"xargs --max-args 1 /usr/bin/sudo id": true, "xargs -P4 -0 /usr/bin/doas id": true, "xargs -- /usr/bin/sudo id": true,
		"xargs -n 1 nice /usr/bin/sudo id": true, "xargs -n 1 grep x": false, "xargs -n 1 -I {} echo {}": false,
	} {
		if got := sudoRuns(cmd, 0); got != want {
			t.Errorf("%q: got %v, want %v", cmd, got, want)
		}
	}
}
