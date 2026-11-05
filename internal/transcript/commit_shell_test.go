package transcript

import "testing"

func TestGitCommitCommandTable(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"timeout 60 git commit -m x", true},
		{"time -p git commit -m x", true},
		{"nice -n 5 git commit -m x", true},
		{"git commit -m x", true},
		{"git -C d commit", true},
		{"sudo -u bob git commit", true},
		{"env FOO=1 git commit", true},
		{`bash -c "git commit -m x"`, true},
		{"echo $(git commit -m x)", true},
		{"nohup git commit &", true},
		{"cat <<EOF\ngit commit\nEOF", false},
		{"echo git commit", false},
		{`grep "git commit" f`, false},
		{"git status", false},
		{"echo x; sudo -u", false},
		{"! git commit", true},
		{"if true; then git commit; fi", true},
	}
	for _, tc := range tests {
		if got := gitCommitCommand(tc.cmd); got != tc.want {
			t.Errorf("gitCommitCommand(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}
