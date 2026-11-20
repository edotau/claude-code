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
	}{{"linux", true, "/mnt/" + winExplorer}, {"linux", false, "xdg-open"}, {"darwin", false, "open"}} {
		openGOOS, openWSL = tc.goos, func() bool { return tc.wsl }
		if got := Opener(); got != tc.want {
			t.Errorf("%s wsl=%v: Opener() = %q, want %q", tc.goos, tc.wsl, got, tc.want)
		}
	}
}

func TestOpenRouteDecisionOrder(t *testing.T) {
	vs := "/home/u/.vscode-server/bin/x/bin/helpers/browser.sh"
	for _, tc := range []struct {
		b       string
		purpose OpenPurpose
		want    string
	}{
		{"none", OpenSignIn, "print"}, {"none", OpenView, "print"},
		{"firefox", OpenSignIn, "browser"}, {vs, OpenView, "browser"},
		{vs, OpenSignIn, "desktop"}, {"", OpenView, "desktop"}, {"", OpenSignIn, "desktop"},
	} {
		if got := openRoute(tc.b, tc.purpose); got != tc.want {
			t.Errorf("openRoute(%q, %v) = %q, want %q", tc.b, tc.purpose, got, tc.want)
		}
	}
}
