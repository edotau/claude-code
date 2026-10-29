package pathview

import (
	"errors"
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
			{Point: "/srv/share one", Kind: KindSMB, Host: "fileserver", RemotePath: "team/docs"},
			{Point: "/srv/nfs", Kind: KindNFS, Host: "nas", RemotePath: "/export/data"},
			{Point: "/home/u/remote", Kind: KindSSHFS, Host: "devbox", RemotePath: "/work"},
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

func TestViewFrom(t *testing.T) {
	r := testResolver()
	for _, tc := range []struct {
		name, local string
		target      Target
		want        string
		err         error
	}{
		{"drive", "/mnt/c/Windows/win.ini", Target{Style: StyleWindows}, `C:\Windows\win.ini`, nil},
		{"drive root", "/mnt/c", Target{Style: StyleWindows}, `C:\`, nil},
		{"wsl-local to windows", "/etc/hostname", Target{Style: StyleWindows}, `\\wsl.localhost\Ubuntu\etc\hostname`, nil},
		{"unc wsl", "/etc/hostname", Target{Style: StyleUNC}, `\\wsl.localhost\Ubuntu\etc\hostname`, nil},
		{"smb unc", "/srv/share one/a/b.txt", Target{Style: StyleUNC}, `\\fileserver\team\docs\a\b.txt`, nil},
		{"smb to windows", "/srv/share one/a", Target{Style: StyleWindows}, `\\fileserver\team\docs\a`, nil},
		{"smb url", "/srv/share one/a b", Target{Style: StyleURL}, "smb://fileserver/team/docs/a%20b", nil},
		{"nfs url", "/srv/nfs/d", Target{Style: StyleURL}, "nfs://nas/export/data/d", nil},
		{"sshfs url", "/home/u/remote/x", Target{Style: StyleURL}, "", ErrNotReachable},
		{"file url", "/etc/hostname", Target{Style: StyleURL}, "file:///etc/hostname", nil},
		{"drive url", "/mnt/c/a", Target{Style: StyleURL}, "file:///C:/a", nil},
		{"server's own view", "/srv/share one/a", Target{Style: StyleUNC, Host: "fileserver"}, `\\fileserver\team\docs\a`, nil},
		{"unknown remote host", "/etc/hostname", Target{Style: StyleWindows, Host: "devbox2"}, "", ErrNotReachable},
		{"host not mount server", "/srv/share one/a", Target{Style: StyleUNC, Host: "other"}, "", ErrNotReachable},
		{"relative", "a/b", Target{Style: StyleWindows}, "", ErrNotReachable},
	} {
		got, err := r.ViewFrom(tc.local, tc.target)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("%s: ViewFrom = %q, %v; want %q, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
	if _, err := (&Resolver{}).ViewFrom("/etc/x", Target{Style: StyleWindows}); !errors.Is(err, ErrNotReachable) {
		t.Errorf("no distro, no mounts: err = %v, want ErrNotReachable", err)
	}
}

func TestRoundTrip(t *testing.T) {
	r := testResolver()
	for _, tc := range []struct {
		in    string
		style Style
	}{
		{`C:\Windows\win.ini`, StyleWindows},
		{`\\wsl.localhost\Ubuntu\etc\hostname`, StyleUNC},
		{`\\wsl.localhost\Ubuntu\etc\hostname`, StyleWindows},
		{`\\fileserver\team\docs\a\b.txt`, StyleUNC},
		{"smb://fileserver/team/docs/a.txt", StyleURL},
		{"nfs://nas/export/data/d/f", StyleURL},
	} {
		local, ok := r.Resolve(tc.in)
		if !ok {
			t.Fatalf("Resolve(%q) failed", tc.in)
		}
		back, err := r.ViewFrom(local, Target{Style: tc.style})
		if err != nil || back != tc.in {
			t.Errorf("round trip %q -> %q -> %q, %v", tc.in, local, back, err)
		}
	}
}

// classified drops opaque entries, leaving the mounts the tables below describe.
func classified(ms []Mount) []Mount {
	var out []Mount
	for _, m := range ms {
		if m.Kind != KindOpaque {
			out = append(out, m)
		}
	}
	return out
}

func TestParseMountinfo(t *testing.T) {
	all := parseMountinfo(mountinfoSample)
	got := classified(all)
	if len(all) != 7 || all[0].Kind != KindOpaque {
		t.Errorf("opaque mounts dropped: %d entries, want 7 incl. / and /proc", len(all))
	}
	want := []Mount{
		{Point: "/mnt/c", Source: `C:\`, FSType: "9p", Kind: KindDrvFS, Drive: "C", Root: "/"},
		{Point: "/mnt/d", Source: `D:\`, FSType: "9p", Kind: KindDrvFS, Drive: "D", Root: "/"},
		{Point: "/srv/share one", Source: "//fileserver/team/docs", FSType: "cifs", Kind: KindSMB, Host: "fileserver", RemotePath: "team/docs"},
		{Point: "/srv/nfs", Source: "nas:/export/data", FSType: "nfs4", Kind: KindNFS, Host: "nas", RemotePath: "/export/data"},
		{Point: "/home/u/remote", Source: "me@devbox:/work", FSType: "fuse.sshfs", Kind: KindSSHFS, Host: "devbox", RemotePath: "/work"},
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
		{Point: "/Volumes/team", Source: "//user@fileserver/team", FSType: "smbfs", Kind: KindSMB, Host: "fileserver", RemotePath: "team"},
		{Point: "/Volumes/data", Source: "nas:/export/data", FSType: "nfs", Kind: KindNFS, Host: "nas", RemotePath: "/export/data"},
		{Point: "/Users/u/remote", Source: "me@devbox:/work", FSType: "macfuse", Kind: KindSSHFS, Host: "devbox", RemotePath: "/work"},
		{Point: "/Volumes/Other Share", Source: "nobody@thing:/x", FSType: "webdav", Kind: KindVolume},
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
	if got, err := r.ViewFrom("/work/a", Target{Style: StyleWindows}); err != nil || got != `C:\Users\u\a` {
		t.Errorf("ViewFrom bind = %q, %v; want C:\\Users\\u\\a", got, err)
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
	r.Mounts = append(r.Mounts, Mount{Point: "/mnt/c/cache", FSType: "tmpfs", Kind: KindOpaque})
	if got, ok := r.Resolve(`C:\cache\a`); ok {
		t.Errorf("Resolve through tmpfs = %q, want not reachable", got)
	}
	if got, ok := r.Resolve(`C:\other\a`); !ok || got != "/mnt/c/other/a" {
		t.Errorf("sibling path = %q, %v", got, ok)
	}
	if got, err := r.ViewFrom("/mnt/c/cache/a", Target{Style: StyleWindows}); err == nil && got == `C:\cache\a` {
		t.Errorf("ViewFrom through tmpfs claimed %q", got)
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
	out, _ := wslpathOut()
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("probe took %s, want < 1.5s", d)
	}
	_ = out
}

func TestViewFromKeepsDotDotThroughSymlinks(t *testing.T) {
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
	got, err := (&Resolver{}).ViewFrom(root+"/link/..", Target{Style: StyleURL})
	if want := "file://" + root + "/real"; err != nil || got != want {
		t.Errorf("ViewFrom = %q, %v; want %q", got, err, want)
	}
}

// An obscured first match must not hide another mount that exposes the same content.
func TestResolveFallsBackToUnobscuredMount(t *testing.T) {
	r := &Resolver{Mounts: []Mount{
		{Point: "/a", Kind: KindSMB, Host: "srv", RemotePath: "team/docs"},
		{Point: "/b", Kind: KindSMB, Host: "srv", RemotePath: "team"},
		{Point: "/a/inner", FSType: "tmpfs", Kind: KindOpaque},
		{Point: "/mnt/c", Kind: KindDrvFS, Drive: "C", Root: "/"},
		{Point: "/mnt/c/Users/u", Kind: KindDrvFS, Drive: "C", Root: "/Users/u"},
		{Point: "/mnt/c/Users/u/x", FSType: "tmpfs", Kind: KindOpaque},
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
