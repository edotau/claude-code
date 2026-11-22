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

// Given a `--host h` before the verb, Then a claude-code commit message or brief is data as in the plain form.
func TestIntentHarnessDataPastHostFlag(t *testing.T) {
	for seg, want := range map[string]bool{
		"claude-code worktree commit -m x": true, "claude-code --host devhost worktree commit -m x": true,
		"claude-code --host=devhost worktree pr": true, "claude-code --host devhost vault get x": false,
		"claude-code --host devhost": false,
	} {
		if got := hardDataSegment(seg, false); got != want {
			t.Errorf("%q: got %v, want %v", seg, got, want)
		}
	}
}

// Given assigned paths, Then $NAME and ${NAME} expand while a longer name sharing the prefix stays.
func TestIntentExpandVarsWholeNames(t *testing.T) {
	got := expandVars("D=/tmp/a; rm $D/x ${D}y $DX $$D")
	if want := "D=/tmp/a; rm /tmp/a/x /tmp/ay $DX $/tmp/a"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
