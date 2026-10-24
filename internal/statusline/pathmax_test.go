package statusline

import (
	"strings"
	"testing"
)

func TestCollapsePath(t *testing.T) {
	cases := []struct {
		name  string
		short string
		cwd   string
		max   int
		want  string
	}{
		{"short path kept", "~/foo", "/home/x/foo", 24, "~/foo"},
		{"over cap collapses to basename", "~/a/b/c/d/e/f/g/h", "/home/x/a/b/c/d/e/f/g/h", 10, "h"},
		{"zero disables the cap", "~/a/b/c/d/e/f/g/h", "/home/x/a/b/c/d/e/f/g/h", 0, "~/a/b/c/d/e/f/g/h"},
		{"exactly at the cap is kept", "0123456789", "/x/0123456789", 10, "0123456789"},
		{"one over the cap collapses", "01234567890", "/x/01234567890", 10, "01234567890"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := collapsePath(c.short, c.cwd, c.max); got != c.want {
				t.Errorf("collapsePath(%q, %q, %d) = %q, want %q", c.short, c.cwd, c.max, got, c.want)
			}
		})
	}
}

// The default config's pathMax (24) collapses a long rendered cwd to the workspace basename end-to-end.
func TestPathMaxDefaultCollapsesLongPath(t *testing.T) {
	hermetic(t)
	payload := `{"model":{"display_name":"Opus"},"workspace":{"current_dir":"/home/x/a/very/deeply/nested/project/workspace"}}`
	got := plain(Render([]byte(payload), "gradient"))
	if !strings.Contains(got, "workspace") || strings.Contains(got, "/a/very/deeply/nested/project/workspace") {
		t.Errorf("long path not collapsed to basename: %q", got)
	}
}
