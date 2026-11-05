package paths

import "testing"

// Within cleans both sides, so a sibling prefix, a trailing slash, ".." and the root "/" all resolve correctly.
func TestWithin(t *testing.T) {
	for _, tc := range []struct {
		path, base string
		want       bool
	}{
		{"/a/foo", "/a/foo", true}, {"/a/foo/bar", "/a/foo", true}, {"/a/foo/", "/a/foo", true},
		{"/a/foobar", "/a/foo", false}, {"/a/foo/../bar", "/a/foo", false}, {"/x", "/", true}, {"/a/foo", "/a/foo/", true},
	} {
		if got := Within(tc.path, tc.base); got != tc.want {
			t.Errorf("Within(%q, %q) = %v, want %v", tc.path, tc.base, got, tc.want)
		}
	}
}
