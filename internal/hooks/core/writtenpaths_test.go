package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
		{"redirect after the destination", `cp a docs/b.md 2>&1`, []string{"/w/docs/b.md"}},
		{"redirect glued to a word", `echo hi>x.md && echo y >|z.md`, []string{"/w/x.md", "/w/z.md"}},
		{"subshell cd does not leak", `(cd /tmp); echo x > a.md`, []string{"/w/a.md"}},
		{"read-only and interpreter bodies", `cat x.go; python3 -c "open('y','w').write('z')"`, nil},
		{"arithmetic > is no redirect", `echo $((3>2))`, nil},
		{"a printed script that runs writes", `printf 'echo x > P.md\n' > /t/s && sh /t/s`, []string{"/t/s", "/w/P.md"}},
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
		{"# after a line continuation is no comment", "echo a\\\n#b > C.md", []string{"/w/C.md"}},
		{"# after a blank and a line continuation is one", "echo a \\\n#b > C.md", nil},
		{"dynamic cd in a subshell does not leak", `(cd "$X"); echo x > S.md`, []string{"/w/S.md"}},
		{"# after an escaped space is no comment", `echo a\ #b; echo x > E.md`, []string{"/w/E.md"}},
		{"# inside ${…} is no comment", `echo ${x:- #} > D.md`, []string{"/w/D.md"}},
		{"strip-comment idiom", `l='a # b'; echo ${l%% #*} > D.md`, []string{"/w/D.md"}},
		{"# after a subshell close is one", `(echo hi)#c > D.md`, nil},
		{"cd - returns to the starting dir", `cd /tmp; cd - && echo x > D.md`, []string{"/w/D.md"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := WrittenPathsPrecise(c.cmd, "/w"); !slices.Equal(got, c.want) {
				t.Errorf("WrittenPathsPrecise(%q) = %q, want %q", c.cmd, got, c.want)
			}
		})
	}
}

// Given every string literal in this package's tests, Then a command with no `<<` passes the heredoc droppers as is.
func TestDropHeredocBodiesNeedsHeredocOperator(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob test files: %v (%d)", err, len(files))
	}
	n := 0
	for _, f := range files {
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || strings.Contains(s, "<<") {
				return true
			}
			n++
			for _, s := range []string{s, s + "\n" + s, "cat > f\n" + s + "\nEOF"} {
				if got := DropDataHeredocs(s); got != s {
					t.Errorf("DropDataHeredocs(%q) = %q, want it unchanged", s, got)
				}
				if got := DropStdinHeredocs(s); got != s {
					t.Errorf("DropStdinHeredocs(%q) = %q, want it unchanged", s, got)
				}
			}
			return true
		})
	}
	if n < 100 {
		t.Fatalf("only %d string literals without `<<`; the scan found too few inputs", n)
	}
}

func TestRemovedPaths(t *testing.T) {
	for cmd, want := range map[string][]string{
		`rm a b`: {"/w/a", "/w/b"}, `mv a b c`: {"/w/a", "/w/b"}, `mv -t /out a b`: {"/w/a", "/w/b"},
		`mv --target-directory=/out a`: {"/w/a"},
	} {
		if got := RemovedPaths(cmd, "/w"); !slices.Equal(got, want) {
			t.Errorf("RemovedPaths(%q) = %q, want %q", cmd, got, want)
		}
	}
}
