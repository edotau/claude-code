package proc

import "testing"

func TestOpenerPicksWindowsBrowserOnWSL(t *testing.T) {
	origOS, origWSL := openGOOS, openWSL
	t.Cleanup(func() { openGOOS, openWSL = origOS, origWSL })
	t.Setenv("PATH", t.TempDir()) // no wslview
	for _, tc := range []struct {
		goos string
		wsl  bool
		want string
	}{{"linux", true, wslExplorer}, {"linux", false, "xdg-open"}, {"darwin", false, "open"}} {
		openGOOS, openWSL = tc.goos, func() bool { return tc.wsl }
		if got := Opener(); got != tc.want {
			t.Errorf("%s wsl=%v: Opener() = %q, want %q", tc.goos, tc.wsl, got, tc.want)
		}
	}
}
