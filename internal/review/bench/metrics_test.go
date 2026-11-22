package bench

import (
	"errors"
	"testing"
)

func TestWithGitErrors(t *testing.T) {
	if e := withGitErrors(Env{Commit: "abc"}, nil, nil); e.Dirty || e.GitError != "" {
		t.Errorf("clean probes: %+v", e)
	}
	if e := withGitErrors(Env{}, nil, errors.New("status failed")); !e.Dirty || e.GitError != "status failed" {
		t.Errorf("failed status: %+v, want dirty with the error kept", e)
	}
}

func TestPickMachine(t *testing.T) {
	cases := []struct{ model, host, want string }{
		{"MacBookPro18,3", "box", "MacBookPro18,3"},
		{"Standard PC (Q35 + ICH9)", "box", "Standard PC (Q35 + ICH9) (box)"},
		{"", "win-host", "win-host"},
		{"", "", "unknown"},
	}
	for _, c := range cases {
		if got := pickMachine(c.model, c.host); got != c.want {
			t.Errorf("pickMachine(%q, %q) = %q, want %q", c.model, c.host, got, c.want)
		}
	}
}
