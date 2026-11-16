package core

import (
	"slices"
	"testing"
)

func TestWrittenPaths(t *testing.T) {
	t.Setenv("HOME", "/h")
	for _, c := range []struct {
		name, cmd string
		want      []string
	}{
		{"sed -i", `sed -i 's/a/b/' x.go`, []string{"/w/x.go"}},
		{"sed -i suffix, two files", `sed -i.bak -e s/a/b/ x.go ../y.go`, []string{"/w/x.go", "/y.go"}},
		{"bsd sed -i ''", `sed -i '' 's/a/b/' x.go`, []string{"/w/x.go"}},
		{"sed -n reads only", `sed -n 5p x.go`, nil},
		{"perl -pi", `perl -pi -e 's/a/b/' x.go`, []string{"/w/x.go"}},
		{"perl without -i reads only", `perl -pe 's/a/b/' x.go`, nil},
		{"redirects", `echo hi > a.md && echo more >>b.md; ls 2>/dev/null`, []string{"/w/a.md", "/w/b.md"}},
		{"stderr and &> files", `make 2>err.log &>all.log`, []string{"/w/err.log", "/w/all.log"}},
		{"fd dup is no file", `make 2>&1 | tee -a out.log`, []string{"/w/out.log"}},
		{"heredoc body is dropped", "cat > n.md <<'EOF'\necho x > not-a-file\nEOF", []string{"/w/n.md"}},
		{"cp destination", `cp src.txt dst.txt`, []string{"/w/dst.txt"}},
		{"mv -t", `mv -t /out a b`, []string{"/out"}},
		{"home and $HOME", `echo x > ~/a && echo y > $HOME/b`, []string{"/h/a", "/h/b"}},
		{"cd moves the anchor", `cd sub && echo x > f`, []string{"/w/sub/f"}},
		{"bash -c body", `bash -c "sed -i s/a/b/ x.go"`, []string{"/w/x.go"}},
		{"read-only and interpreter bodies", `cat x.go; python3 -c "open('y','w').write('z')"`, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := WrittenPaths(c.cmd, "/w"); !slices.Equal(got, c.want) {
				t.Errorf("WrittenPaths(%q) = %q, want %q", c.cmd, got, c.want)
			}
		})
	}
}

func TestWrittenPathsPrecise(t *testing.T) {
	for _, c := range []struct {
		name, cmd string
		want      []string
	}{
		{"quoted >", `grep -n '>' N.md`, nil},
		{"quoted > in a commit message", `git commit -m x -m '> moved H.md'`, nil},
		{"escaped >", `echo a \> b.md`, nil},
		{"comment", `ls # then > N.md`, nil},
		{"comment hides a chained redirect", `ls # build && copy > N.md`, nil},
		{"stdin heredoc to gh", "gh issue create --body-file - <<'EOF'\n> N.md is stale\nEOF", nil},
		{"heredoc piped to gh", "cat <<EOF | gh pr comment 1 -F -\n> N.md\nEOF", nil},
		{"python heredoc", "python3 - <<'EOF'\n# > O.md\nEOF", nil},
		{"while-read heredoc", "while read l; do echo $l; done <<'EOF'\n> W.md\nEOF", nil},
		{"dynamic cd", `cd "$DIR" && printf x > N.md`, nil},
		{"cd to a substitution", `cd "$(mktemp -d)" && echo x > N.md`, nil},
		{"never more than WrittenPaths", "python3 - <<'EOF'\ncd /tmp\nEOF\necho x > A.md", nil},
		{"real redirect beside a quoted >", `grep '>' a > R.md # c`, []string{"/w/a", "/w/R.md"}},
		{"shell-fed heredoc body", "bash <<'EOF'\necho x > B.md\nEOF", []string{"/w/B.md"}},
		{"absolute target after a dynamic cd", `cd "$X" && echo x > /r/A.md`, []string{"/r/A.md"}},
		{"# after an escaped space is no comment", `echo a\ #b; echo x > E.md`, []string{"/w/E.md"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := WrittenPathsPrecise(c.cmd, "/w"); !slices.Equal(got, c.want) {
				t.Errorf("WrittenPathsPrecise(%q) = %q, want %q", c.cmd, got, c.want)
			}
		})
	}
}
