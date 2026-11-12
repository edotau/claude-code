package paths

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const mountinfoSample = `22 1 8:1 / / rw,relatime - ext4 /dev/sdc rw
30 22 0:28 / /mnt/c rw,noatime - 9p C:\134 rw,aname=drvfs;path=C:\;uid=1000
31 22 0:29 / /mnt/d rw - 9p D:\134 rw,aname=drvfs
40 22 0:40 / /srv/share\040one rw - cifs //fileserver/team/docs rw,vers=3.0
41 22 0:41 / /srv/nfs rw shared:5 - nfs4 nas:/export/data rw
42 22 0:42 / /home/u/remote rw - fuse.sshfs me@devbox:/work rw,user_id=1000
43 22 0:43 / /proc rw - proc proc rw
`

const darwinSample = `/dev/disk3s1s1 on / (apfs, sealed, local, read-only, journaled)
devfs on /dev (devfs, local, nobrowse)
//user@fileserver/team on /Volumes/team (smbfs, nodev, nosuid, mounted by user)
nas:/export/data on /Volumes/data (nfs, nodev, nosuid, mounted by user)
me@devbox:/work on /Users/u/remote (macfuse, nodev, nosuid, synchronous, mounted by u)
map auto_home on /System/Volumes/Data/home (autofs, automounted, nobrowse)
nobody@thing:/x on /Volumes/Other Share (webdav, nodev)
/dev/disk4s1 on /Volumes/USB (msdos, local)
`

func testResolver() *Resolver {
	return &Resolver{
		Distro: "Ubuntu",
		Home:   "/home/u",
		Mounts: []Mount{
			{Point: "/mnt/c", Kind: KindDrvFS, Drive: "C"},
			{Point: "/srv/share one", Kind: kindSMB, Host: "fileserver", RemotePath: "team/docs"},
			{Point: "/srv/nfs", Kind: KindNFS, Host: "nas", RemotePath: "/export/data"},
			{Point: "/home/u/remote", Kind: kindSSHFS, Host: "devbox", RemotePath: "/work"},
		},
		LookupHome: func(n string) (string, bool) { return "/home/" + n, n == "bob" },
	}
}

func TestResolve(t *testing.T) {
	r := testResolver()
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{`C:\Windows\win.ini`, "/mnt/c/Windows/win.ini", true},
		{`c:/Windows/win.ini`, "/mnt/c/Windows/win.ini", true},
		{`"C:\Program Files\x"`, "/mnt/c/Program Files/x", true},
		{`'C:\a'`, "/mnt/c/a", true},
		{`Z:\nope`, "", false},
		{`\\wsl.localhost\Ubuntu\etc\hostname`, "/etc/hostname", true},
		{`\\wsl$\ubuntu\etc`, "/etc", true},
		{`\\wsl.localhost\Debian\etc`, "", false},
		{`\\fileserver\team\docs\a\b.txt`, "/srv/share one/a/b.txt", true},
		{`\\FILESERVER\Team\docs`, "/srv/share one", true},
		{`\\fileserver\team\other`, "", false},
		{`\\stranger\team\docs`, "", false},
		{"file:///etc/hostname", "/etc/hostname", true},
		{"file:///C:/Windows/win.ini", "/mnt/c/Windows/win.ini", true},
		{"file://localhost/etc/x%20y", "/etc/x y", true},
		{"file://fileserver/team/docs/f", "/srv/share one/f", true},
		{"~", "/home/u", true},
		{"~/a/../b", "/home/u/b", true},
		{"~bob/x", "/home/bob/x", true},
		{"~nobody/x", "", false},
		{"smb://user@fileserver/team/docs/a.txt", "/srv/share one/a.txt", true},
		{"nfs://nas/export/data/d/f", "/srv/nfs/d/f", true},
		{"nfs://nas/other", "", false},
		{"nas:/export/data/d", "/srv/nfs/d", true},
		{"me@devbox:/work/src/x.go", "/home/u/remote/src/x.go", true},
		{"devbox:/elsewhere", "", false},
		{"/usr//bin/../lib", "/usr/lib", true},
		{"relative/path", "", false},
		{"", "", false},
	} {
		got, ok := r.Resolve(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Resolve(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestWindowsPath(t *testing.T) {
	r := testResolver()
	for _, tc := range []struct {
		local, want string
		ok          bool
	}{
		{"/mnt/c/Windows/win.ini", `C:\Windows\win.ini`, true},
		{"/mnt/c", `C:\`, true},
		{"/etc/hostname", "", false},
		{"/srv/share one/a", "", false},
		{"mnt/c/a", "", false},
	} {
		if got, ok := r.WindowsPath(tc.local); got != tc.want || ok != tc.ok {
			t.Errorf("WindowsPath(%q) = %q, %v; want %q, %v", tc.local, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseSSHFSUserIPv6(t *testing.T) {
	got := parseMountinfo("50 22 0:50 / /srv/ssh rw - fuse.sshfs alice@[2001:db8::1]:/work rw\n")
	want := Mount{
		Point: "/srv/ssh", Kind: kindSSHFS, Host: "2001:db8::1", RemotePath: "/work",
	}
	if len(got) != 1 {
		t.Fatalf("mounts = %+v, want one SSHFS mount", got)
	}
	if got[0] != want {
		t.Fatalf("SSHFS mount = %+v, want %+v", got[0], want)
	}
}

func TestResolveUnknownSlashUNC(t *testing.T) {
	r := testResolver()
	for _, input := range []string{"//stranger/team/docs", "//fileserver/team/other", "//wsl.localhost/Debian/etc"} {
		if got, ok := r.Resolve(input); ok || got != "" {
			t.Errorf("Resolve(%q) = %q, %v; unknown UNC must not become a local path", input, got, ok)
		}
	}
	if got, ok := r.Resolve("//fileserver/team/docs/a"); !ok || got != "/srv/share one/a" {
		t.Fatalf("known UNC = %q, %v", got, ok)
	}
}

// classified drops opaque entries, leaving the mounts the tables below describe.
func classified(ms []Mount) []Mount {
	var out []Mount
	for _, m := range ms {
		if m.Kind != kindOpaque {
			out = append(out, m)
		}
	}
	return out
}

func TestParseMountinfo(t *testing.T) {
	all := parseMountinfo(mountinfoSample)
	got := classified(all)
	if len(all) != 7 || all[0].Kind != kindOpaque {
		t.Errorf("opaque mounts dropped: %d entries, want 7 incl. / and /proc", len(all))
	}
	want := []Mount{
		{Point: "/mnt/c", Kind: KindDrvFS, Drive: "C", Root: "/"},
		{Point: "/mnt/d", Kind: KindDrvFS, Drive: "D", Root: "/"},
		{Point: "/srv/share one", Kind: kindSMB, Host: "fileserver", RemotePath: "team/docs"},
		{Point: "/srv/nfs", Kind: KindNFS, Host: "nas", RemotePath: "/export/data"},
		{Point: "/home/u/remote", Kind: kindSSHFS, Host: "devbox", RemotePath: "/work"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d mounts, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mount %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseDarwinMount(t *testing.T) {
	got := classified(parseDarwinMount(darwinSample))
	want := []Mount{
		{Point: "/Volumes/team", Kind: kindSMB, Host: "fileserver", RemotePath: "team"},
		{Point: "/Volumes/data", Kind: KindNFS, Host: "nas", RemotePath: "/export/data"},
		{Point: "/Users/u/remote", Kind: kindSSHFS, Host: "devbox", RemotePath: "/work"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d mounts, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mount %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// A macOS SMB share resolves through the parsed table.
	r := &Resolver{Mounts: classified(got)}
	if p, ok := r.Resolve(`\\fileserver\team\x.txt`); !ok || p != "/Volumes/team/x.txt" {
		t.Errorf("darwin smb resolve = %q, %v", p, ok)
	}
}

func TestLearnDistroFromWslpath(t *testing.T) {
	for out, want := range map[string]string{
		"\\\\wsl.localhost\\Ubuntu\\\n": "Ubuntu",
		"\\\\wsl$\\Debian-12\\":         "Debian-12",
		"C:\\\n":                        "",
		"":                              "",
	} {
		if got := distroFromUNC(out); got != want {
			t.Errorf("distroFromUNC(%q) = %q, want %q", out, got, want)
		}
	}
	saved := wslpathOut
	t.Cleanup(func() { wslpathOut = saved })
	wslpathOut = func() (string, error) { return "\\\\wsl.localhost\\Ubuntu\\\n", nil }
	if got := distroFromUNC(mustProbe()); got != "Ubuntu" {
		t.Errorf("probe path = %q", got)
	}
}

func mustProbe() string { s, _ := wslpathOut(); return s }

func TestBindMountRoot(t *testing.T) {
	m := parseMountinfo(`50 22 0:30 /Users/u /work rw - 9p C:\134 rw,aname=drvfs` + "\n" +
		`51 22 0:31 /export/sub /srv/sub rw - nfs4 nas:/export/data rw`)
	r := &Resolver{Mounts: m, Distro: "Ubuntu"}
	if got, ok := r.WindowsPath("/work/a"); !ok || got != `C:\Users\u\a` {
		t.Errorf("WindowsPath bind = %q, %v; want C:\\Users\\u\\a", got, ok)
	}
	if got, ok := r.Resolve(`C:\Users\u\a`); !ok || got != "/work/a" {
		t.Errorf("Resolve bind = %q, %v; want /work/a", got, ok)
	}
	if _, ok := r.Resolve(`C:\Other\a`); ok {
		t.Error("a path outside the bound subtree resolved")
	}
	if got, ok := r.Resolve("nfs://nas/export/data/export/sub/f"); !ok || got != "/srv/sub/f" {
		t.Errorf("Resolve nfs bind = %q, %v", got, ok)
	}
}

func TestDriveDotDotClampsAtRoot(t *testing.T) {
	r := testResolver()
	if got, ok := r.Resolve(`C:\..\etc\hostname`); !ok || got != "/mnt/c/etc/hostname" {
		t.Errorf("Resolve = %q, %v; want /mnt/c/etc/hostname", got, ok)
	}
}

func TestDotDotFollowsSymlinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "real", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real", "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real", "sub"), filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	got, ok := testResolver().Resolve(filepath.Join(root, "link") + "/../f")
	if !ok || got != filepath.Join(root, "real", "f") {
		t.Errorf("Resolve = %q, %v; want %s", got, ok, filepath.Join(root, "real", "f"))
	}
}

// A tmpfs (opaque) mount inside a drive is not the drive's content: not reachable, never a guess.
func TestOpaqueMountFencesDrive(t *testing.T) {
	r := testResolver()
	r.Mounts = append(r.Mounts, Mount{Point: "/mnt/c/cache", Kind: kindOpaque})
	if got, ok := r.Resolve(`C:\cache\a`); ok {
		t.Errorf("Resolve through tmpfs = %q, want not reachable", got)
	}
	if got, ok := r.Resolve(`C:\other\a`); !ok || got != "/mnt/c/other/a" {
		t.Errorf("sibling path = %q, %v", got, ok)
	}
	if got, ok := r.WindowsPath("/mnt/c/cache/a"); ok {
		t.Errorf("WindowsPath through tmpfs claimed %q", got)
	}
}

func TestDotDotFollowsSymlinksInEveryForm(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "real", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real", "sub"), filepath.Join(root, "link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	r := &Resolver{Home: root, Distro: "Ubuntu"}
	want := filepath.Join(root, "real")
	for _, in := range []string{"~/link/..", "file://" + root + "/link/..", `\\wsl.localhost\Ubuntu` + root + "/link/.."} {
		if got, ok := r.Resolve(in); !ok || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %s", in, got, ok, want)
		}
	}
}

func TestLearnDistroSurvivesStdoutHoldingChild(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 5 &\nprintf '\\\\\\\\wsl.localhost\\\\Ubuntu\\\\'\n"
	if err := os.WriteFile(filepath.Join(dir, "wslpath"), []byte(script), 0o700); err != nil {
		t.Skip(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	start := time.Now()
	_, _ = wslpathOut()
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("probe took %s, want < 1.5s", d)
	}
}

// An obscured first match must not hide another mount that exposes the same content.
func TestResolveFallsBackToUnobscuredMount(t *testing.T) {
	r := &Resolver{Mounts: []Mount{
		{Point: "/a", Kind: kindSMB, Host: "srv", RemotePath: "team/docs"},
		{Point: "/b", Kind: kindSMB, Host: "srv", RemotePath: "team"},
		{Point: "/a/inner", Kind: kindOpaque},
		{Point: "/mnt/c", Kind: KindDrvFS, Drive: "C", Root: "/"},
		{Point: "/mnt/c/Users/u", Kind: KindDrvFS, Drive: "C", Root: "/Users/u"},
		{Point: "/mnt/c/Users/u/x", Kind: kindOpaque},
	}}
	if got, ok := r.Resolve(`\\srv\team\docs\inner\f`); !ok || got != "/b/docs/inner/f" {
		t.Errorf("smb fallback = %q, %v; want /b/docs/inner/f", got, ok)
	}
	if got, ok := r.Resolve(`C:\Users\u\x\f`); ok {
		t.Errorf("drive fully obscured resolved to %q", got)
	}
	if got, ok := r.Resolve(`C:\Users\u\y`); !ok || got != "/mnt/c/Users/u/y" {
		t.Errorf("drive specific = %q, %v", got, ok)
	}
}

func TestRemoteDotDotFollowsLocalSymlinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"real/sub", "data"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "real", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real", "sub"), filepath.Join(root, "data", "link")); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "real", "f")
	cases := map[string]Mount{
		"nas:/export/data/link/../f":     {Point: root, Kind: KindNFS, Host: "nas", RemotePath: "/export"},
		"me@devbox:/work/data/link/../f": {Point: root, Kind: kindSSHFS, Host: "devbox", RemotePath: "/work"},
	}
	for in, m := range cases {
		r := &Resolver{Mounts: []Mount{m}}
		got, ok := r.Resolve(in)
		if !ok || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestNinePUNCClassifiedSMB(t *testing.T) {
	ms := parseMountinfo("50 22 0:50 / /mnt/srv rw - 9p \\134\\134fileserver\\134team rw,aname=drvfs\n")
	if len(ms) != 1 || ms[0].Kind != kindSMB || ms[0].Host != "fileserver" || ms[0].RemotePath != "team" {
		t.Fatalf("mounts = %+v", ms)
	}
	r := &Resolver{Mounts: ms}
	got, ok := r.Resolve(`\\fileserver\team\file`)
	if !ok || got != "/mnt/srv/file" {
		t.Errorf("Resolve = %q, %v", got, ok)
	}
}

func TestRemoteDotDotCannotLeaveMountRoot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"mnt/nas", "mnt/export"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"mnt/nas/file", "mnt/export/file"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := &Resolver{Mounts: []Mount{{Point: filepath.Join(root, "mnt/nas"), Kind: KindNFS, Host: "nas", RemotePath: "/export"}}}
	got, ok := r.Resolve("nas:/export/../export/file")
	if want := filepath.Join(root, "mnt/nas/file"); !ok || got != want {
		t.Errorf("Resolve = %q, %v; want %q", got, ok, want)
	}
}

func TestRemoteDotDotSymlinkCannotEscapeMount(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"nas", "other/sub"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "other", "secret"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "other", "sub"), filepath.Join(root, "nas", "link")); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Mounts: []Mount{
		{Point: filepath.Join(root, "nas"), Kind: KindNFS, Host: "nas", RemotePath: "/export"},
		{Point: filepath.Join(root, "other"), Kind: kindOpaque},
	}}
	if got, ok := r.Resolve("nas:/export/link/../secret"); ok {
		t.Errorf("Resolve escaped the mount: %q", got)
	}
}
