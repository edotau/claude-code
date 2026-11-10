package opencode

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

	"github.com/edotau/claude-code/internal/paths/pathstest"
)

// fakeOpencode swaps the exec seam for a scripted opencode: version answers --version, list answers
// `auth list` (listErr makes it fail). It isolates PATH/XDG_DATA_HOME, resets the caches, and returns the
// data home plus a pointer counting `auth list` calls.
func fakeOpencode(t *testing.T, version, list string, listErr error) (dataHome string, listCalls *int) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dataHome = t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("PATH", t.TempDir())
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

// opencodeMajor reads the major from the first --version line only; a line with no version reads as 0.
func TestOpencodeMajorParsesVersionLine(t *testing.T) {
	for out, want := range map[string]int{"1.19.3": 1, "v2.0.32": 2, "opencode 2.1.0\n3.0.0": 2, "banner\n2.0.0": 0, "garbage": 0} {
		fakeOpencode(t, out, "", nil)
		if got, err := opencodeMajor(); err != nil || got != want {
			t.Errorf("--version %q: opencodeMajor() = %d, %v; want %d", out, got, err, want)
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
	bin := pathstest.FakeVendor(t, "opencode", "exit 0")
	if got, err := OpencodeLogoutArgv("openai", "cred_2"); err != nil || !reflect.DeepEqual(got, []string{bin, "auth", "logout", "openai", "cred_2"}) {
		t.Errorf("2.x logout = %v, err = %v", got, err)
	}
	if got, err := OpencodeSwitchArgv("openai", "cred_2"); err != nil || !reflect.DeepEqual(got, []string{bin, "auth", "switch", "openai", "cred_2"}) {
		t.Errorf("2.x switch = %v, err = %v", got, err)
	}
	fakeOpencode(t, "1.19.3", "", nil) // fakeOpencode resets CLAUDE_CONFIG_DIR, so bin/opencode must be re-registered
	bin = pathstest.FakeVendor(t, "opencode", "exit 0")
	if got, err := OpencodeLogoutArgv("openai", "openai"); err != nil || !reflect.DeepEqual(got, []string{bin, "auth", "logout", "openai"}) {
		t.Errorf("1.x logout = %v, err = %v", got, err)
	}
	if got, err := OpencodeSwitchArgv("openai", "openai"); err != nil || got != nil {
		t.Errorf("1.x switch = %v, err = %v, want nil, nil", got, err)
	}
}

// TestOpencodeArgvErrorsWithoutFallingBackToBarePATHLookup: a missing bin/opencode must surface
// AgentBin's "not installed" error, never the bare "opencode" string this used to fall back to.
func TestOpencodeArgvErrorsWithoutFallingBackToBarePATHLookup(t *testing.T) {
	fakeOpencode(t, "2.0.32", "[]", nil) // CLAUDE_CONFIG_DIR is fresh here: bin/opencode was never written
	if got, err := OpencodeLogoutArgv("openai", "cred_2"); err == nil || got != nil {
		t.Errorf("logout = %v, err = %v, want (nil, AgentBin error)", got, err)
	} else if strings.Contains(err.Error(), `"opencode"`) || err.Error() == "opencode" {
		t.Errorf("err = %v, must not be the bare PATH fallback", err)
	}
	if got, err := OpencodeSwitchArgv("openai", "cred_2"); err == nil || got != nil {
		t.Errorf("switch = %v, err = %v, want (nil, AgentBin error)", got, err)
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

// fakeOpencodeOnDisk adds an installed binary for cache keys and counts every exec through the seam.
func fakeOpencodeOnDisk(t *testing.T, version, list string, listErr error) (dataHome, bin string, execs *int) {
	t.Helper()
	dataHome, _ = fakeOpencode(t, version, list, listErr)
	bin = pathstest.FakeVendor(t, "opencode", "")
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

// TestOpencodeAuthMemoryCacheDBAndWALChangeRelists pins same-process refreshes after another terminal changes 2.x auth.
func TestOpencodeAuthMemoryCacheDBAndWALChangeRelists(t *testing.T) {
	data, _, execs := fakeOpencodeOnDisk(t, "2.0.32", v2List, nil)
	db := filepath.Join(data, "opencode", "opencode.db")
	touch(t, db, "db", time.Unix(1000, 0))
	authOnce(t)
	touch(t, db, "db", time.Unix(2000, 0))
	authOnce(t)
	if *execs != 3 {
		t.Fatalf("after db change execs = %d, want 3 (same-process list re-run)", *execs)
	}
	touch(t, db+"-wal", "wal", time.Unix(3000, 0))
	authOnce(t)
	if *execs != 4 {
		t.Fatalf("after -wal change execs = %d, want 4", *execs)
	}
	authOnce(t)
	if *execs != 4 {
		t.Fatalf("unchanged db execs = %d, want 4 (memory cache reused)", *execs)
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
	if c := loadOpencodeDiskCache(); c.Auth == nil {
		t.Fatalf("cache not rewritten: %+v", c)
	}
}

func TestOpencodeDiskCacheFailedListNotCached(t *testing.T) {
	_, _, execs := fakeOpencodeOnDisk(t, "2.0.32", "", errors.New("exit status 1"))
	if _, err := OpencodeAuth(); err == nil {
		t.Fatal("want the auth list failure")
	}
	if c := loadOpencodeDiskCache(); c.Auth != nil {
		t.Fatalf("cache = %+v, want no auth entry", c)
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
	if c := loadOpencodeDiskCache(); c.Auth != nil {
		t.Fatalf("after invalidate cache = %+v, want no auth entry", c)
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
	if c := loadOpencodeDiskCache(); c.Auth != nil {
		t.Fatalf("1.x cache = %+v, want no auth entry", c)
	}
}

// TestOpencodeMajorFailedProbeNotCached: a failed --version probe returns 0 once, and the next call re-probes.
func TestOpencodeMajorFailedProbeNotCached(t *testing.T) {
	fakeOpencode(t, "", "[]", nil)
	if got := OpencodeMajor(); got != 0 {
		t.Fatalf("failed probe major = %d, want 0", got)
	}
	opencodeOutput = func(_ context.Context, args ...string) ([]byte, error) { return []byte("2.0.32\n"), nil }
	if got := OpencodeMajor(); got != 2 {
		t.Errorf("major after recovery = %d, want 2 (the failure was cached)", got)
	}
}

// A major-0 --version answer is reused in-process but never persisted, so a fresh process re-probes it.
func TestOpencodeMajorZeroNotPersisted(t *testing.T) {
	_, _, execs := fakeOpencodeOnDisk(t, "dev-build", "[]", nil)
	for range 2 {
		if got := OpencodeMajor(); got != 0 {
			t.Fatalf("major = %d, want 0", got)
		}
	}
	if *execs != 1 {
		t.Fatalf("same-process execs = %d, want 1 (0 cached in memory)", *execs)
	}
	newProcess(t)
	OpencodeMajor()
	if *execs != 2 {
		t.Fatalf("fresh-process execs = %d, want 2 (0 never persisted)", *execs)
	}
}

// An installed opencode whose --version fails surfaces that probe error through OpencodeAuth.
func TestOpencodeAuthReportsVersionProbeError(t *testing.T) {
	fakeOpencodeOnDisk(t, "", "[]", nil)
	if _, err := OpencodeAuth(); err == nil || !strings.Contains(err.Error(), "opencode --version") {
		t.Fatalf("OpencodeAuth err = %v, want the --version probe error", err)
	}
}
