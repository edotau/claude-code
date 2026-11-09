package shell

import "testing"

// mktemp must not create files outside $TMPDIR or the roots.
func TestCommandMktempScoped(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		cmd  string
		want bool
	}{
		{`mktemp "${TMPDIR:-/tmp}/task.XXXXXX"`, true},
		{`mktemp "$TMPDIR/task.XXXXXX"`, true},
		{`mktemp ./task.XXXXXX`, true},
		{`mktemp -d ./work.XXXXXX`, true},
		{"mktemp " + root + "/task.XXXXXX", true},
		{`mktemp /etc/XXXXXX`, false},
		{`mktemp ../../x.XXXXXX`, false},
		{`mktemp -p /etc x.XXXXXX`, false},
		{`mktemp --tmpdir=/etc x.XXXXXX`, false},
	}
	for _, c := range cases {
		if got := CommandIn(c.cmd, root, []string{root}, nil); got != c.want {
			t.Errorf("CommandIn(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}
