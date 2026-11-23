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
		b           string
		purpose     OpenPurpose
		vscode, ssh bool
		want        string
	}{
		{"none", OpenView, true, false, "print"}, {"firefox", OpenSignIn, true, true, "browser"},
		{vs, OpenView, true, true, "vscode"}, {vs, OpenSignIn, true, true, "desktop"},
		{"", OpenSignIn, true, true, "desktop"}, {"", OpenView, true, false, "vscode"},
		{"", OpenView, false, false, "desktop"}, {"", OpenView, false, true, "print"},
	} {
		if got := openRoute(tc.b, tc.purpose, tc.vscode, tc.ssh); got != tc.want {
			t.Errorf("openRoute(%q, %v, vscode=%v, ssh=%v) = %q, want %q", tc.b, tc.purpose, tc.vscode, tc.ssh, got, tc.want)
		}
	}
}

func TestDeviceHostURL(t *testing.T) {
	orig := DeviceHosts
	t.Cleanup(func() { DeviceHosts = orig })
	DeviceHosts = func() []string { return []string{"Dbx.corp.example.com"} }
	for u, want := range map[string]bool{
		"https://adb-1.2.azuredatabricks.net/jobs":          true,
		"https://dbc-ab.cloud.databricks.com/":              true,
		"https://accounts.cloud.databricks.com/login":       true,
		"https://dbx.corp.example.com/":                     true,
		"https://login.microsoftonline.com/x/oauth2":        true,
		"https://microsoft.com/devicelogin":                 true,
		"https://aka.ms/devicelogin":                        true,
		"https://oauth-openshift.apps.c1.example.com/oauth": true,
		"https://evil-databricks.com/":                      false,
		"https://databricks.com.evil.io/":                   false,
		"https://example.com/devicelogin":                   false,
		"https://github.com/x":                              false,
	} {
		if got := deviceHostURL(u); got != want {
			t.Errorf("deviceHostURL(%q) = %v, want %v", u, got, want)
		}
	}
	for _, u := range []string{"https://github.com/x", "https://dbc-ab.cloud.databricks.com/"} {
		purpose := OpenView
		if deviceHostURL(u) {
			purpose = OpenSignIn
		}
		want := map[bool]string{true: "desktop", false: "vscode"}[purpose == OpenSignIn]
		if got := openRoute("", purpose, true, true); got != want {
			t.Errorf("route %q = %q, want %q", u, got, want)
		}
	}
}
