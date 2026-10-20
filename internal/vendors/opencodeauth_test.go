package vendors

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeOpencode swaps the exec seam for a scripted opencode: version answers --version, list answers
// `auth list` (listErr makes it fail). It isolates PATH/XDG_DATA_HOME, resets the caches, and returns the
// data home plus a pointer counting `auth list` calls.
func fakeOpencode(t *testing.T, version, list string, listErr error) (dataHome string, listCalls *int) {
	t.Helper()
	dataHome = t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("OPENCODE_DB", "")
	calls := 0
	orig := opencodeOutput
	opencodeOutput = func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) == 1 && args[0] == "--version":
			if version == "" {
				return nil, exec.ErrNotFound
			}
			return []byte(version + "\n"), nil
		case len(args) >= 2 && args[0] == "auth" && args[1] == "list":
			calls++
			want := []string{"auth", "list", "--format", "json", "--standalone"}
			if !reflect.DeepEqual(args, want) {
				t.Errorf("auth list args = %v, want %v", args, want)
			}
			return []byte(list), listErr
		}
		t.Errorf("unexpected opencode call %v", args)
		return nil, errors.New("unexpected")
	}
	resetOpencodeCaches()
	t.Cleanup(func() { opencodeOutput = orig; resetOpencodeCaches() })
	return dataHome, &calls
}

func resetOpencodeCaches() {
	opencodeMu.Lock()
	opencodeMajorSet, opencodeAuthIsSet = false, false
	opencodeAuthVal, opencodeAuthErr = nil, nil
	opencodeMu.Unlock()
}

func writeAuthJSON(t *testing.T, dataHome, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dataHome, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataHome, "opencode", "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const v2List = `[
  {"id":"openai","name":"OpenAI","connections":[
    {"type":"credential","id":"cred_2","label":"work","method":"oauth","active":true,"extra":1},
    {"type":"credential","id":"cred_3","label":"home","method":"key","status":{"status":"needs_auth","message":"sign in again","url":"https://x"}}]},
  {"id":"github-copilot","name":"GitHub Copilot","future":{"a":1},"connections":[
    {"type":"env","name":"GITHUB_TOKEN"}]}
]`

func TestParseOpencodeMajor(t *testing.T) {
	for in, want := range map[string]int{
		"1.19.3": 1, "2.0.32": 2, "v2.0.32": 2, "opencode 2.1.0\n": 2, "garbage": 0, "": 0,
	} {
		if got := parseOpencodeMajor(in); got != want {
			t.Errorf("parseOpencodeMajor(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestOpencodeMajorFromSeam(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    int
	}{{"1.19.3", 1}, {"2.0.32", 2}, {"not a version", 0}, {"", 0}} {
		fakeOpencode(t, tc.version, "[]", nil)
		if got := OpencodeMajor(); got != tc.want {
			t.Errorf("version %q: OpencodeMajor() = %d, want %d", tc.version, got, tc.want)
		}
	}
}

func TestOpencodeAuthV2(t *testing.T) {
	data, _ := fakeOpencode(t, "2.0.32", v2List, nil)
	writeAuthJSON(t, data, `{"stale":{"type":"api"}}`) // 2.x ignores auth.json when its CLI answers
	got, err := OpencodeAuth()
	if err != nil {
		t.Fatal(err)
	}
	want := []OpencodeIntegration{
		{ID: "github-copilot", Name: "GitHub Copilot", Conns: []OpencodeConn{{Kind: "env", Label: "GITHUB_TOKEN"}}},
		{ID: "openai", Name: "OpenAI", Conns: []OpencodeConn{
			{Kind: "credential", ID: "cred_2", Label: "work", Method: "oauth", Active: true},
			{Kind: "credential", ID: "cred_3", Label: "home", Method: "key", NeedsAuth: "sign in again"},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OpencodeAuth() =\n%+v\nwant\n%+v", got, want)
	}
	if l := OpencodeLogins(); !reflect.DeepEqual(l, []string{"github-copilot", "openai"}) {
		t.Errorf("OpencodeLogins() = %v", l)
	}
	if !OpencodeCopilotLoginPresent() {
		t.Error("an env-backed github-copilot connection counts as a login")
	}
}

func TestOpencodeAuthV2FailureFallsBackToAuthJSON(t *testing.T) {
	data, _ := fakeOpencode(t, "2.0.32", "", errors.New("exit status 1"))
	writeAuthJSON(t, data, `{"github-copilot":{"type":"oauth"}}`)
	got, err := OpencodeAuth()
	if err == nil || !strings.Contains(err.Error(), "opencode auth list") {
		t.Fatalf("err = %v, want the 2.x failure", err)
	}
	if len(got) != 1 || got[0].ID != "github-copilot" {
		t.Fatalf("fallback = %+v, want auth.json's github-copilot", got)
	}
	if l := OpencodeLogins(); !reflect.DeepEqual(l, []string{"github-copilot"}) {
		t.Errorf("OpencodeLogins() = %v", l)
	}

	fakeOpencode(t, "2.0.32", "not json", nil)
	if _, err := OpencodeAuth(); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("bad JSON err = %v, want a decode error", err)
	}
}

func TestOpencodeAuthV1(t *testing.T) {
	data, calls := fakeOpencode(t, "1.19.3", "", nil)
	writeAuthJSON(t, data, `{"openai/":{"type":"oauth","access":"SECRET"},"nvidia":{"type":"api","key":"SECRET"},"wk":{"type":"wellknown"}}`)
	got, err := OpencodeAuth()
	if err != nil {
		t.Fatal(err)
	}
	want := []OpencodeIntegration{
		{ID: "nvidia", Name: "nvidia", Conns: []OpencodeConn{{Kind: "credential", ID: "nvidia", Label: "API key", Method: "key"}}},
		{ID: "openai", Name: "openai", Conns: []OpencodeConn{{Kind: "credential", ID: "openai", Label: "OAuth", Method: "oauth"}}},
		{ID: "wk", Name: "wk", Conns: []OpencodeConn{{Kind: "credential", ID: "wk", Label: "API key", Method: "key"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OpencodeAuth() =\n%+v\nwant\n%+v", got, want)
	}
	if strings.Contains(strings.Join(OpencodeLogins(), ""), "SECRET") {
		t.Error("credential values must never surface")
	}
	if *calls != 0 {
		t.Errorf("1.x must not call `auth list`; calls = %d", *calls)
	}
}

func TestOpencodeAuthAbsent(t *testing.T) {
	fakeOpencode(t, "", "", nil)
	got, err := OpencodeAuth()
	if got != nil || err != nil {
		t.Fatalf("no binary, no auth.json: got %v, %v; want nil, nil", got, err)
	}
	if OpencodeLogins() != nil {
		t.Error("OpencodeLogins() should be nil")
	}
}

func TestOpencodeLoginsSameOnBothVersions(t *testing.T) {
	data, _ := fakeOpencode(t, "1.19.3", "", nil)
	writeAuthJSON(t, data, `{"github-copilot":{"type":"oauth"},"openai":{"type":"api"}}`)
	v1 := OpencodeLogins()
	fakeOpencode(t, "2.0.32", `[{"id":"openai","name":"OpenAI","connections":[{"type":"credential","id":"c1","label":"a","method":"key"}]},
		{"id":"github-copilot","name":"GitHub Copilot","connections":[{"type":"credential","id":"c2","label":"b","method":"oauth"}]},
		{"id":"empty","name":"Empty","connections":[]}]`, nil)
	v2 := OpencodeLogins()
	want := []string{"github-copilot", "openai"}
	if !reflect.DeepEqual(v1, want) || !reflect.DeepEqual(v2, want) {
		t.Fatalf("v1 = %v, v2 = %v, want %v on both", v1, v2, want)
	}
}

func TestOpencodeArgvPerVersion(t *testing.T) {
	fakeOpencode(t, "2.0.32", "[]", nil)
	if got := OpencodeLogoutArgv("openai", "cred_2"); !reflect.DeepEqual(got, []string{"opencode", "auth", "logout", "openai", "cred_2"}) {
		t.Errorf("2.x logout = %v", got)
	}
	if got := OpencodeSwitchArgv("openai", "cred_2"); !reflect.DeepEqual(got, []string{"opencode", "auth", "switch", "openai", "cred_2"}) {
		t.Errorf("2.x switch = %v", got)
	}
	fakeOpencode(t, "1.19.3", "", nil)
	if got := OpencodeLogoutArgv("openai", "openai"); !reflect.DeepEqual(got, []string{"opencode", "auth", "logout", "openai"}) {
		t.Errorf("1.x logout = %v", got)
	}
	if got := OpencodeSwitchArgv("openai", "openai"); got != nil {
		t.Errorf("1.x switch = %v, want nil", got)
	}
}

func TestOpencodeAuthCacheAndInvalidate(t *testing.T) {
	_, calls := fakeOpencode(t, "2.0.32", `[{"id":"a","name":"A","connections":[{"type":"env","name":"A_KEY"}]}]`, nil)
	for range 3 {
		if _, err := OpencodeAuth(); err != nil {
			t.Fatal(err)
		}
	}
	if *calls != 1 {
		t.Fatalf("auth list calls = %d, want 1 (cached)", *calls)
	}
	InvalidateOpencodeAuth()
	if _, err := OpencodeAuth(); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatalf("auth list calls after invalidate = %d, want 2", *calls)
	}
}

// A 1.x auth.json rewrite (e.g. by `opencode auth login` in another terminal) misses the cache without Invalidate.
func TestOpencodeAuthV1SeesFileChange(t *testing.T) {
	data, _ := fakeOpencode(t, "1.19.3", "", nil)
	if OpencodeLogins() != nil {
		t.Fatal("want no logins yet")
	}
	writeAuthJSON(t, data, `{"anthropic":{"type":"api"}}`)
	if l := OpencodeLogins(); !reflect.DeepEqual(l, []string{"anthropic"}) {
		t.Fatalf("OpencodeLogins() = %v", l)
	}
}

// fakeOpencodeOnDisk is fakeOpencode plus a stat-able opencode binary on PATH (so the disk cache keys),
// returning the binary path and a counter of every exec through the seam.
func fakeOpencodeOnDisk(t *testing.T, version, list string, listErr error) (dataHome, bin string, execs *int) {
	t.Helper()
	dataHome, _ = fakeOpencode(t, version, list, listErr)
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	bin = filepath.Join(dir, "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	n := 0
	inner := opencodeOutput
	opencodeOutput = func(ctx context.Context, args ...string) ([]byte, error) { n++; return inner(ctx, args...) }
	t.Cleanup(func() { opencodeOutput = inner })
	return dataHome, bin, &n
}

// newProcess drops the in-memory caches, as a fresh claude-code process would start.
func newProcess(t *testing.T) { t.Helper(); resetOpencodeCaches() }

func touch(t *testing.T, path, body string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func authOnce(t *testing.T) []OpencodeIntegration {
	t.Helper()
	ints, err := OpencodeAuth()
	if err != nil {
		t.Fatal(err)
	}
	return ints
}

func TestOpencodeDiskCacheV2SkipsExec(t *testing.T) {
	data, _, execs := fakeOpencodeOnDisk(t, "2.0.32", v2List, nil)
	touch(t, filepath.Join(data, "opencode", "opencode.db"), "db", time.Unix(1000, 0))
	first := authOnce(t)
	if *execs != 2 {
		t.Fatalf("first process execs = %d, want 2 (--version + auth list)", *execs)
	}
	newProcess(t)
	if got := authOnce(t); !reflect.DeepEqual(got, first) {
		t.Fatalf("cached = %+v, want %+v", got, first)
	}
	if *execs != 2 {
		t.Fatalf("second process execs = %d, want 2 (served from disk)", *execs)
	}
	fi, err := os.Stat(opencodeCacheFile())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("cache mode = %v, want 0600", fi.Mode().Perm())
	}
	if di, _ := os.Stat(filepath.Dir(opencodeCacheFile())); di.Mode().Perm() != 0o700 {
		t.Errorf("cache dir mode = %v, want 0700", di.Mode().Perm())
	}
}

func TestOpencodeDiskCacheBinaryChangeReprobes(t *testing.T) {
	_, bin, execs := fakeOpencodeOnDisk(t, "2.0.32", v2List, nil)
	authOnce(t)
	if err := os.Chtimes(bin, time.Unix(5000, 0), time.Unix(5000, 0)); err != nil {
		t.Fatal(err)
	}
	newProcess(t)
	authOnce(t)
	if *execs != 4 {
		t.Fatalf("execs = %d, want 4 (binary change re-probes version and list)", *execs)
	}
}

func TestOpencodeDiskCacheDBAndWALChangeRelists(t *testing.T) {
	data, _, execs := fakeOpencodeOnDisk(t, "2.0.32", v2List, nil)
	db := filepath.Join(data, "opencode", "opencode.db")
	touch(t, db, "db", time.Unix(1000, 0))
	authOnce(t)
	touch(t, db, "db", time.Unix(2000, 0))
	newProcess(t)
	authOnce(t)
	if *execs != 3 {
		t.Fatalf("after db change execs = %d, want 3 (version cached, list re-run)", *execs)
	}
	touch(t, db+"-wal", "wal", time.Unix(3000, 0))
	newProcess(t)
	authOnce(t)
	if *execs != 4 {
		t.Fatalf("after -wal change execs = %d, want 4", *execs)
	}
	newProcess(t)
	authOnce(t)
	if *execs != 4 {
		t.Fatalf("unchanged db execs = %d, want 4", *execs)
	}
}

func TestOpencodeDiskCacheHonorsOpencodeDB(t *testing.T) {
	_, _, execs := fakeOpencodeOnDisk(t, "2.0.32", v2List, nil)
	db := filepath.Join(t.TempDir(), "custom.db")
	t.Setenv("OPENCODE_DB", db)
	touch(t, db, "db", time.Unix(1000, 0))
	authOnce(t)
	if a := loadOpencodeDiskCache().Auth; a == nil || !strings.HasPrefix(a.DB, db+"|") {
		t.Fatalf("auth entry = %+v, want keyed on %s", a, db)
	}
	touch(t, db, "db2", time.Unix(1000, 0))
	newProcess(t)
	authOnce(t)
	if *execs != 3 {
		t.Fatalf("execs = %d, want 3 (absolute OPENCODE_DB change re-lists)", *execs)
	}

	t.Setenv("OPENCODE_DB", ":memory:")
	newProcess(t)
	authOnce(t)
	newProcess(t)
	authOnce(t)
	if *execs != 5 {
		t.Fatalf("in-memory DB execs = %d, want 5 (auth never served from disk)", *execs)
	}
}

func TestOpencodeDiskCacheCorruptFallsBackToProbe(t *testing.T) {
	_, _, execs := fakeOpencodeOnDisk(t, "2.0.32", v2List, nil)
	touch(t, opencodeCacheFile(), "{not json", time.Now())
	newProcess(t)
	if got := authOnce(t); len(got) != 2 {
		t.Fatalf("got %+v, want the live list", got)
	}
	if *execs != 2 {
		t.Fatalf("execs = %d, want 2 (corrupt cache ignored)", *execs)
	}
	if c := loadOpencodeDiskCache(); c.Version == nil || c.Auth == nil {
		t.Fatalf("cache not rewritten: %+v", c)
	}
}

func TestOpencodeDiskCacheFailedListNotCached(t *testing.T) {
	_, _, execs := fakeOpencodeOnDisk(t, "2.0.32", "", errors.New("exit status 1"))
	if _, err := OpencodeAuth(); err == nil {
		t.Fatal("want the auth list failure")
	}
	if c := loadOpencodeDiskCache(); c.Auth != nil || c.Version == nil {
		t.Fatalf("cache = %+v, want version only", c)
	}
	newProcess(t)
	OpencodeAuth()
	if *execs != 3 {
		t.Fatalf("execs = %d, want 3 (failed list retried, version from disk)", *execs)
	}
}

func TestOpencodeDiskCacheInvalidateDropsAuth(t *testing.T) {
	_, _, execs := fakeOpencodeOnDisk(t, "2.0.32", v2List, nil)
	authOnce(t)
	InvalidateOpencodeAuth()
	if c := loadOpencodeDiskCache(); c.Auth != nil || c.Version == nil {
		t.Fatalf("after invalidate cache = %+v, want version only", c)
	}
	newProcess(t)
	authOnce(t)
	if *execs != 3 {
		t.Fatalf("execs = %d, want 3 (list re-run, version kept)", *execs)
	}
}

func TestOpencodeDiskCacheV1VersionOnly(t *testing.T) {
	data, _, execs := fakeOpencodeOnDisk(t, "1.19.3", "", nil)
	writeAuthJSON(t, data, `{"openai":{"type":"oauth"}}`)
	authOnce(t)
	newProcess(t)
	if got := authOnce(t); len(got) != 1 || got[0].ID != "openai" {
		t.Fatalf("got %+v", got)
	}
	if *execs != 1 {
		t.Fatalf("execs = %d, want 1 (version from disk on the second process)", *execs)
	}
	if c := loadOpencodeDiskCache(); c.Auth != nil || c.Version == nil || c.Version.Major != 1 {
		t.Fatalf("1.x cache = %+v, want version only", c)
	}
}
