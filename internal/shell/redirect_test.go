package shell

import "testing"

// Given segments, Then only an unquoted redirect that can write a file counts; descriptor duplication does not.
func TestFileRedirect(t *testing.T) {
	cases := map[string]bool{
		"go test ./x 2>&1": false, "go test >&2": false, "go test 1>&2": false, "go test 2>&-": false, "go test 2>&1 ": false,
		"(go test 2>&1)": false, `echo "a > b"`: false, "echo 'a>b'": false, `echo a\>b`: false, "echo $'a>b'": false,
		"go test": false, "echo a < b": false,
		"go test > f": true, "go test >> f": true, "go test >| f": true, "go test &> f": true, "go test 2> f": true,
		"go test >&f": true, "go test >&": true, "go test 2>&1 > f": true, "go test >&2f": true, "go test <> f": true,
		"tee >(cat)": true, `echo "x" > f`: true, `echo "$(x)" >f`: true,
		"go test 2>&1\t": false, "echo >": true, "go test 2>&1>": true, `echo \\>f`: true,
	}
	for seg, want := range cases {
		if got := FileRedirect(seg); got != want {
			t.Errorf("FileRedirect(%q) = %v, want %v", seg, got, want)
		}
	}
}
