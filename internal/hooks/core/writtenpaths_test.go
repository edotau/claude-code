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
