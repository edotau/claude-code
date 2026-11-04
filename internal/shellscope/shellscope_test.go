package shellscope

import "testing"

// TestGitCommitAllowedRefusesReuse pins that -c/-C (reuse another commit's message) are refused, clustered or not.
func TestGitCommitAllowedRefusesReuse(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"-c", "HEAD", "-m", "x"}, false},
		{[]string{"-C", "HEAD"}, false},
		{[]string{"-cHEAD"}, false},
		{[]string{"-m", "x"}, true},
	}
	for _, c := range cases {
		if got := gitCommitAllowed(c.args); got != c.want {
			t.Errorf("gitCommitAllowed(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}
