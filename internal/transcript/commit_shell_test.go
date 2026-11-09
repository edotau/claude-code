package transcript

import "testing"

func TestCountEditsHeredocTerminatorWhitespace(t *testing.T) {
	runCommitCases(t, []commitCase{
		{"plain leading space", bashLine("cat <<'EOF'\n EOF\ngit commit -m x\nEOF"), false},
		{"plain leading tab", bashLine("cat <<'EOF'\n\tEOF\ngit commit -m x\nEOF"), false},
		{"plain trailing space", bashLine("cat <<'EOF'\nEOF \ngit commit -m x\nEOF"), false},
		{"tab stripping leading space", bashLine("cat <<-'EOF'\n EOF\ngit commit -m x\nEOF"), false},
		{"tab stripping leading tab", bashLine("cat <<-'EOF'\n\tEOF\ngit commit -m x"), true},
		{"exact terminator", bashLine("cat <<'EOF'\nEOF\ngit commit -m x"), true},
	})
}

func TestCountEditsBuiltinAliasPrecedence(t *testing.T) {
	runCommitCases(t, []commitCase{
		{"rev-parse builtin", bashLine("git -c alias.rev-parse=commit rev-parse --is-inside-work-tree"), false},
		{"hash-object builtin", bashLine("git -c alias.hash-object=commit hash-object file.txt"), false},
		{"custom commit alias", bashLine("git -c alias.ci=commit ci -m x"), true},
	})
}

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
		{"echo \"see <<EOF here\"\ngit commit -m x", true},
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
