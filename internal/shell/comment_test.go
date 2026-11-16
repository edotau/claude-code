package shell

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestCommentScanMatchesReference holds the one-pass scanner to the per-query CommentAt it replaced.
func TestCommentScanMatchesReference(t *testing.T) {
	corpus := testStrings(t, ".", "../hooks/core", "../hooks/safety", "../agent")
	strs, positions := 0, 0
	for _, s := range corpus {
		strs++
		positions += compareComments(t, s)
	}
	t.Logf("%d strings, %d `#` positions compared", strs, positions)
	if strs < 1000 || positions < 50 {
		t.Fatalf("only %d strings and %d positions: the corpus scan found too few", strs, positions)
	}
}

// TestCommentScanRandom compares the scanner and the reference on short strings over the bytes the rules read.
func TestCommentScanRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	words := []string{"#", " ", "'", `"`, `\`, "$", "(", ")", "{", "}", "\n", ";", "|", "&", "x", "`", ">", "case",
		"${", "$((", "))", "$'", "#)"}
	strs, positions := 0, 0
	for range 200000 {
		var b strings.Builder
		for range 1 + r.IntN(16) {
			b.WriteString(words[r.IntN(len(words))])
		}
		strs++
		positions += compareComments(t, b.String())
	}
	t.Logf("%d strings, %d `#` positions compared", strs, positions)
}

// compareComments checks At at every `#`, exprMask and DropComments of s; it returns the positions compared.
func compareComments(t *testing.T, s string) int {
	t.Helper()
	n := 0
	idx := ScanComments(s)
	for i := range len(s) {
		if s[i] != '#' {
			continue
		}
		n++
		if want := refCommentAt(s, i); idx.At(i) != want {
			t.Errorf("At(%q, %d) = %v, want %v", s, i, idx.At(i), want)
		}
	}
	if got, want := exprMask(s), refExprMask(s); !slices.Equal(got, want) {
		t.Errorf("exprMask(%q) = %v, want %v", s, got, want)
	}
	if got, want := DropComments(s), refDropComments(s); got != want {
		t.Errorf("DropComments(%q) = %q, want %q", s, got, want)
	}
	return n
}

// testStrings are the string literals (and + chains of them) in the test files of dirs.
func testStrings(t *testing.T, dirs ...string) []string {
	t.Helper()
	var out []string
	fset := token.NewFileSet()
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("test files in %s: %v", dir, err)
		}
		for _, f := range files {
			file, err := parser.ParseFile(fset, f, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				if s, ok := literal(n); ok {
					out = append(out, s)
				}
				return true
			})
		}
	}
	return out
}

func literal(n ast.Node) (string, bool) {
	switch e := n.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		l, lok := literal(e.X)
		r, rok := literal(e.Y)
		return l + r, lok && rok && e.Op == token.ADD
	case *ast.ParenExpr:
		return literal(e.X)
	}
	return "", false
}

// The reference: CommentAt, exprMask, arithEnd, groupClose and core's dropComments as of 54017b9c.

func refDropComments(cmd string) string {
	for from := 0; ; {
		at, i := QuoteAt(cmd), -1
		for k := from; k < len(cmd) && i < 0; k++ {
			if cmd[k] == '#' && at[k] == 0 && refCommentAt(cmd, k) {
				i = k
			}
		}
		if i < 0 {
			return cmd
		}
		end := strings.IndexByte(cmd[i:], '\n')
		if end < 0 {
			return cmd[:i]
		}
		cmd, from = cmd[:i]+cmd[i+end:], i+1
	}
}

func refCommentAt(cmd string, i int) bool {
	if refExprMask(cmd)[i] {
		return false
	}
	for i > 0 {
		c := cmd[i-1]
		if strings.IndexByte(" \t\n;&|()", c) < 0 {
			return false
		}
		if !escaped(cmd, i-1) {
			return c != ')' || refGroupClose(cmd, i-1)
		}
		if c != '\n' {
			return false
		}
		i -= 2
	}
	return true
}

func refExprMask(cmd string) []bool {
	at, mask := QuoteAt(cmd), make([]bool, len(cmd))
	for i := 0; i+1 < len(cmd); i++ {
		if at[i] != 0 || cmd[i] != '$' {
			continue
		}
		end := braceExpansionEnd(cmd, i)
		if end < 0 && strings.HasPrefix(cmd[i+1:], "((") {
			end = refArithEnd(cmd, at, i)
		}
		for k := i; k <= end; k++ {
			mask[k] = true
		}
		i = max(i, end)
	}
	return mask
}

func refArithEnd(cmd string, at []byte, i int) int {
	depth := 0
	for j := i + 1; j < len(cmd); j++ {
		if at[j] != 0 {
			continue
		}
		switch cmd[j] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 1 {
				if j+1 < len(cmd) && cmd[j+1] == ')' {
					return j + 1
				}
				return -1
			}
		}
	}
	return -1
}

func refGroupClose(cmd string, j int) bool {
	if strings.Contains(cmd, "case") {
		return false
	}
	at, mask := QuoteAt(cmd), refExprMask(cmd)
	var open []bool
	for k := 0; k <= j; k++ {
		if at[k] != 0 || mask[k] {
			continue
		}
		switch cmd[k] {
		case '(':
			open = append(open, k == 0 || at[k-1] == 0 && strings.IndexByte(" \t\n;&|(", cmd[k-1]) >= 0)
		case ')':
			if len(open) == 0 {
				return false
			}
			bare := open[len(open)-1]
			if open = open[:len(open)-1]; k == j {
				return bare
			}
		}
	}
	return false
}

// benchCommands are a 210 KB command with 3,000 comments and a 5,000-line unquoted-heredoc markdown write.
func benchCommands() map[string]string {
	var c, h strings.Builder
	for i := range 3000 {
		fmt.Fprintf(&c, "echo step %d > /tmp/out_%d.txt # note %d about the step's output\n", i, i, i)
	}
	h.WriteString("cat > /tmp/notes/docs/notes.md <<EOF\n")
	for i := range 5000 {
		if i%10 == 0 {
			fmt.Fprintf(&h, "## Section %d (part %d)\n", i, i/10)
		} else {
			fmt.Fprintf(&h, "Line %d of the doc: it's plain text with a #tag and notes.\n", i)
		}
	}
	h.WriteString("EOF\n")
	return map[string]string{"comments": c.String(), "heredoc": h.String()}
}

func BenchmarkCommentsEachHash(b *testing.B) {
	for name, cmd := range benchCommands() {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				idx := ScanComments(cmd)
				for i := range len(cmd) {
					if cmd[i] == '#' {
						idx.At(i)
					}
				}
			}
		})
	}
}
