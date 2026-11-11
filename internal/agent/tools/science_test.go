package tools

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClaudeScienceAssetName(t *testing.T) {
	setupToolsTestEnv(t)
	for _, tc := range []struct{ goos, goarch, want string }{
		{"linux", "amd64", "linux-x64"},
		{"darwin", "arm64", "mac-arm64.dmg"},
		{"darwin", "amd64", "mac-x64.dmg"},
		{"windows", "amd64", ""},
	} {
		if got := claudeScienceAssetName(tc.goos, tc.goarch); got != tc.want {
			t.Errorf("claudeScienceAssetName(%s,%s) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

func TestInstallClaudeScienceDryRunTouchesNoNetwork(t *testing.T) {
	setupToolsTestEnv(t)
	old := claudeScienceBaseURL
	claudeScienceBaseURL = "http://127.0.0.1:1/claude-science"
	defer func() { claudeScienceBaseURL = old }()
	var buf bytes.Buffer
	if err := installClaudeScience(context.Background(), &buf, localBinDir(), true); err != nil {
		t.Fatalf("dry-run must not touch the network: %v", err)
	}
	if !strings.Contains(buf.String(), "dry-run") {
		t.Errorf("output = %q, want a dry-run marker", buf.String())
	}
}

// stubScienceTools fakes hdiutil/codesign/ditto: attach builds an app at the mountpoint, signed by team.
func stubScienceTools(t *testing.T, team string) {
	t.Helper()
	old := scienceRun
	t.Cleanup(func() { scienceRun = old })
	scienceRun = func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch {
		case name == "hdiutil" && args[0] == "attach":
			cli := filepath.Join(args[len(args)-2], scienceAppName, "Contents/Resources/bin/claude-science")
			if err := os.MkdirAll(filepath.Dir(cli), 0o755); err != nil {
				return nil, err
			}
			return nil, os.WriteFile(cli, []byte("#!/bin/sh\necho claude-science 9.9\n"), 0o755)
		case name == "codesign" && args[0] == "-dv":
			return []byte("Identifier=x\nTeamIdentifier=" + team + "\n"), nil
		case name == "ditto":
			return exec.Command("cp", "-R", args[0], args[1]).CombinedOutput()
		}
		return nil, nil
	}
}

func runScienceInstall(t *testing.T, team string) (root, bin string, err error) {
	setupToolsTestEnv(t)
	if runtime.GOOS == "windows" {
		t.Skip("the stubbed app is an sh script reached through a symlink")
	}
	oldGOOS, oldGOARCH := goosOverride, goarchOverride
	goosOverride, goarchOverride = "darwin", "arm64" // every macOS tool is stubbed, so any unix host runs it
	t.Cleanup(func() { goosOverride, goarchOverride = oldGOOS, oldGOARCH })
	stubScienceTools(t, team)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("dmg")) }))
	t.Cleanup(srv.Close)
	oldURL, oldRoots := claudeScienceBaseURL, scienceAppRoots
	root, bin = filepath.Join(t.TempDir(), "Applications"), t.TempDir()
	claudeScienceBaseURL, scienceAppRoots = srv.URL, func() []string { return []string{root} }
	t.Cleanup(func() { claudeScienceBaseURL, scienceAppRoots = oldURL, oldRoots })
	return root, bin, installClaudeScience(context.Background(), &bytes.Buffer{}, bin, false)
}

func TestInstallClaudeScienceDarwinInstallsAppAndLinksCLI(t *testing.T) {
	root, bin, err := runScienceInstall(t, scienceTeamID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join(bin, "claude-science"))
	if want := filepath.Join(root, scienceAppName, "Contents/Resources/bin/claude-science"); err != nil || got != want {
		t.Fatalf("link = %q, %v; want %q", got, err, want)
	}
	if out, err := exec.Command(filepath.Join(bin, "claude-science")).CombinedOutput(); err != nil || !strings.Contains(string(out), "9.9") {
		t.Errorf("run via link = %q, %v", out, err)
	}
}

func TestInstallClaudeScienceDarwinRefusesForeignSigner(t *testing.T) {
	root, bin, err := runScienceInstall(t, "OTHERTEAM")
	if err == nil || !strings.Contains(err.Error(), "not signed by team") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	for _, dir := range []string{root, bin} {
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s holds %d entries after a refused install", dir, len(entries))
		}
	}
}

func TestScienceRollbackFailureStopsFallback(t *testing.T) {
	setupToolsTestEnv(t)
	stubScienceTools(t, scienceTeamID)
	base, fallback := t.TempDir(), t.TempDir()
	prior := filepath.Join(base, scienceAppName)
	writeToolsFixture(t, filepath.Join(prior, "prior"), "original app", 0o644)
	root := filepath.Join(t.TempDir(), "Applications")
	// Moving the old app invalidates this alias, so both the swap and restore fail without races.
	symlinkToolsFixture(t, prior+string(os.PathSeparator)+"..", root)
	oldRoots := scienceAppRoots
	scienceAppRoots = func() []string { return []string{root, fallback} }
	t.Cleanup(func() { scienceAppRoots = oldRoots })

	var out bytes.Buffer
	dest, err := installAppFromDMG(context.Background(), &out, "fixture.dmg", t.TempDir())
	if dest != "" || !errors.Is(err, errAppRollback) {
		t.Fatalf("install returned %q, %v; want rollback failure instead of fallback success; output: %s", dest, err, &out)
	}
	if _, statErr := os.Stat(filepath.Join(fallback, scienceAppName)); !os.IsNotExist(statErr) {
		t.Fatalf("fallback was attempted after failed rollback: %v", statErr)
	}
	assertSciencePreviousAppPreserved(t, base, err)
}

func assertSciencePreviousAppPreserved(t *testing.T, base string, installErr error) {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".claude-science-") {
		t.Fatalf("expected one preserved staging directory, got %v", entries)
	}
	old := filepath.Join(entries[0].Name(), "old.app")
	got, err := os.ReadFile(filepath.Join(base, old, "prior"))
	if err != nil || string(got) != "original app" {
		t.Fatalf("displaced app = %q, %v; want original app preserved", got, err)
	}
	if !strings.Contains(installErr.Error(), old) {
		t.Fatalf("rollback error does not identify displaced app %q: %v", old, installErr)
	}
}
