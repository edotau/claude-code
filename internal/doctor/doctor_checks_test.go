package doctor

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/gemini" // registers the gemini route engine the prod binary links
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestMemoryDreamRowFlagsFailedAndStale(t *testing.T) {
	now := time.Date(2026, 11, 25, 14, 2, 0, 0, time.Local)
	ok := memory.DreamResult{At: now.Add(-time.Hour), Outcome: memory.DreamOK,
		Agent: "claude", Profile: "dev", Trigger: "schedule"}
	cases := []struct {
		name, detail string
		pass         bool
		runs         []memory.DreamResult
	}{
		{"last failed beats recent success", "last run failed 2026-11-25 14:02: auth; see dream-20261125.log, local model", false,
			[]memory.DreamResult{ok, {At: now, Outcome: memory.DreamFailed, Reason: "auth", Log: "/tmp/dream-20261125.log", Local: true}}},
		{"stale noop", "no ok run since 2026-11-23 13:02 (>48h)", false,
			[]memory.DreamResult{{At: now.Add(-49 * time.Hour), Outcome: memory.DreamNoop}}},
		{"recent local noop", "last ok 2026-11-25 13:02 on claude·dev (schedule), local model", true,
			[]memory.DreamResult{{At: ok.At, Outcome: memory.DreamNoop, Agent: ok.Agent, Profile: ok.Profile, Trigger: ok.Trigger, Local: true}}},
		{"skipped is not success", "no ok run yet (memory dream --force)", false,
			[]memory.DreamResult{{At: now, Outcome: memory.DreamNoop, Reason: "not due"}}},
		{"skipped after failure", "last run failed 2026-11-25 14:02: auth", false,
			[]memory.DreamResult{{At: now, Outcome: memory.DreamFailed, Reason: "auth"},
				{At: now, Outcome: memory.DreamNoop, Reason: "not due"}}},
		{"skips do not hide success", "last ok 2026-11-25 13:02 on claude·dev (schedule)", true,
			append([]memory.DreamResult{ok}, repeatedDreamSkips(now, 60)...)},
		{"no record", "no record yet (memory dream --force)", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeDreamResults(t, tc.runs...)
			row := memoryDreamRow(now)
			if row.Name != "memory dream" || row.OK != tc.pass || row.Hard || row.Detail != tc.detail {
				t.Fatalf("row = %+v, want %q (ok=%t)", row, tc.detail, tc.pass)
			}
		})
	}
}

func repeatedDreamSkips(at time.Time, n int) []memory.DreamResult {
	rs := make([]memory.DreamResult, n)
	for i := range rs {
		rs[i] = memory.DreamResult{At: at, Outcome: memory.DreamNoop, Reason: "not due"}
	}
	return rs
}

func writeDreamResults(t *testing.T, rs ...memory.DreamResult) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if len(rs) == 0 {
		return
	}
	var lines []byte
	for _, r := range rs {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(append(lines, b...), '\n')
	}
	seedHarness(t, "memory/dream-results.jsonl", string(lines))
}

func TestStrayClaudeJSONRejectsNonENOENTStatFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	hosttest.SymlinkLoop(t, filepath.Join(home, ".claude"))
	stub := filepath.Join(home, ".claude", ".claude.json")
	ok, detail := strayClaudeJSON()
	if ok || !strings.Contains(detail, "cannot stat "+stub) {
		t.Fatalf("stray config probe = %v, %q; want unhealthy cannot-stat diagnostic", ok, detail)
	}
}

func TestHookBinaryStatus(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if ok, d := hookBinaryStatus(dir); ok || !strings.Contains(d, "missing") {
		t.Fatalf("no .hookd: %v %q", ok, d)
	}
	write(paths.HookBinaryName, "ELF..stamp-a..")
	if ok, d := hookBinaryStatus(dir); ok || !strings.Contains(d, "no .build-stamp") {
		t.Fatalf("no stamp: %v %q", ok, d)
	}
	write(paths.StampFile, "stamp-b\n")
	if ok, d := hookBinaryStatus(dir); ok || !strings.Contains(d, "not built with stamp stamp-b") {
		t.Fatalf("stale: %v %q", ok, d)
	}
	write(paths.StampFile, "stamp-a 1 2\n")
	if ok, d := hookBinaryStatus(dir); !ok || !strings.Contains(d, "matches stamp stamp-a") {
		t.Fatalf("match: %v %q", ok, d)
	}
}

func TestRepoRootCheck(t *testing.T) {
	const pass = "no test binaries or profiles"
	cases := []struct {
		name, want string
		make       []string
	}{
		{"empty", pass, nil},
		{"leftovers", "warn 2 leftover file(s): cpu.prof, memory.test; delete them (go test -o <scratch>)",
			[]string{"memory.test", "cpu.prof", "go.mod"}},
		{"dirs ignored", pass, []string{"x.test/", "sub/", "sub/y.test"}},
	}
	for _, tc := range cases {
		root := t.TempDir()
		for _, f := range tc.make {
			p := filepath.Join(root, f)
			if strings.HasSuffix(f, "/") {
				_ = os.MkdirAll(p, 0o755)
			} else if err := os.WriteFile(p, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if c := repoRootCheck(root); c.Detail != tc.want || c.Hard || c.OK != (tc.want == pass) {
			t.Errorf("%s: row = %+v, want %q", tc.name, c, tc.want)
		}
	}
}

func TestFormatsCheckFailsOnUnreadableStore(t *testing.T) {
	hermetic(t)
	if err := os.MkdirAll(filepath.Dir(vault.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vault.Path(), bytes.Repeat([]byte("not a bolt db"), 400), 0o600); err != nil {
		t.Fatal(err)
	}
	c := formatsCheck()
	if c.OK || !strings.Contains(c.Detail, "unreadable") {
		t.Fatalf("formats row = %+v, want failing with 'unreadable'", c)
	}
}

func TestStrayClaudeJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	if ok, _ := strayClaudeJSON(); !ok {
		t.Fatal("no stub must pass")
	}
	stub := filepath.Join(home, ".claude", ".claude.json")
	if err := os.MkdirAll(filepath.Dir(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, detail := strayClaudeJSON(); ok || !strings.Contains(detail, stub) {
		t.Errorf("stub present: ok=%v detail=%q", ok, detail)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if ok, detail := strayClaudeJSON(); !ok {
		t.Errorf("custom config dir flagged ~/.claude/.claude.json: %q", detail)
	}
}

func TestVaultDoctorChecks(t *testing.T) {
	t.Run("missing vault", testVaultDoctorMissing)
	t.Run("secrets and retired", testVaultDoctorSecrets)
	t.Run("corrupt retired metadata", testVaultDoctorCorrupt)
	t.Run("one vault row, not two", testVaultDoctorSingleRow)
}

func testVaultDoctorMissing(t *testing.T) {
	t.Helper()
	hermetic(t)
	checks := vaultDoctorChecks()
	c := findCheck(checks, "vault")
	if c == nil || c.Detail != "not created yet" || !c.OK {
		t.Errorf("vault row = %+v", c)
	}
	for _, name := range []string{"vault perms", "keyring tool"} {
		if c := findCheck(checks, name); c == nil || !c.OK {
			t.Errorf("missing healthy %s row", name)
		}
	}
}

func testVaultDoctorSecrets(t *testing.T) {
	t.Helper()
	hermetic(t)
	t.Setenv("HARNESS_VAULT_KEY", "")
	vaulttest.Keyring(t)
	vaulttest.Secret(t, "FOO_API_KEY", "sk-test-0123456789")
	if _, _, err := vault.RotateKey(false); err != nil {
		t.Fatal(err)
	}
	c := findCheck(vaultDoctorChecks(), "vault")
	if c == nil || !c.OK {
		t.Fatalf("vault row = %+v", c)
	}
	for _, want := range []string{vault.Path(), "key keyring", "1 secret(s)", "1 retired"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q lacks %q", c.Detail, want)
		}
	}
	if strings.Contains(c.Detail, "sk-test") {
		t.Error("doctor printed a secret")
	}
}

func testVaultDoctorCorrupt(t *testing.T) {
	t.Helper()
	hermetic(t)
	vaulttest.Put(t, vault.Meta, "retired", `["old", 42]`)
	checks := vaultDoctorChecks()
	for _, name := range []string{"vault", "vault retired keys"} {
		c := findCheck(checks, name)
		if c == nil || c.OK || !strings.Contains(c.Detail, "retired key IDs are corrupt") {
			t.Errorf("%s row = %+v, want corruption diagnostic", name, c)
		}
	}
}

func testVaultDoctorSingleRow(t *testing.T) {
	t.Helper()
	hermetic(t)
	t.Setenv("HARNESS_VAULT_KEY", "")
	vaulttest.Keyring(t)
	vaulttest.Secret(t, "FOO_API_KEY", "sk-test-0123456789")
	n := 0
	for _, c := range vaultDoctorChecks() {
		if c.Name == "vault" || c.Name == "vault master key" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("vaultDoctorChecks has %d vault/master-key row(s), want exactly 1 (the \"vault\" row carries the master-key status)", n)
	}
}

func seedHarness(t *testing.T, name, body string) {
	t.Helper()
	p := filepath.Join(paths.StateDir(), name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFormatsDoctorRowIgnoresBareMapCaches(t *testing.T) {
	hermetic(t)
	seedHarness(t, "quota.json", `{"acct":{"until":"2099-01-01T00:00:00Z"}}`)
	seedHarness(t, "agentversion.json", `{"claude":"1.0.0"}`) // unversioned cache: a miss, never stale

	formats := findCheck([]check{formatsCheck()}, "formats")
	if formats == nil || !formats.OK || strings.Contains(formats.Detail, "stale") { // quota stays a bare map: an old daemon reads it
		t.Fatalf("formats row = %+v, want current", formats)
	}
}

func TestMemorySyncRows(t *testing.T) {
	at := time.Date(2026, 11, 18, 9, 30, 0, 0, time.Local)
	st := &memory.SyncState{Peers: map[string]*memory.PeerSync{
		"ok.example":    {Pull: memory.SyncDir{OK: at, Summary: "3 files", Differ: 2}, Push: memory.SyncDir{OK: at}},
		"sleep.example": {Pull: memory.SyncDir{OK: at}, Unreachable: at},
		"bad.example":   {Pull: memory.SyncDir{Refusal: "layout differs"}},
	}}
	cases := []struct {
		name, on, hosts string
		want            []check
	}{
		{"off", "", "ok.example", []check{{Name: "memory sync", OK: true, Detail: "off: not opted in (vault env NETWORK_MEMORY)"}}},
		{"no peer", "on", "", []check{{Name: "memory sync", OK: true, Detail: "on; CLAUDE_REMOTE_HOST names no peer"}}},
		{"healthy", "on", "ok.example", []check{{Name: "memory sync ok.example", OK: true, Detail: "pulled 2026-11-18 09:30; pushed 2026-11-18 09:30; differ 2; 3 files"}}},
		{"unreachable", "on", "sleep.example", []check{{Name: "memory sync sleep.example", OK: true, Detail: "unreachable since 2026-11-18 09:30; pulled 2026-11-18 09:30"}}},
		{"refused", "on", "bad.example", []check{{Name: "memory sync bad.example", OK: false, Detail: "refused: layout differs; never synced"}}},
		{"never", "on", "new.example", []check{{Name: "memory sync new.example", OK: true, Detail: "never synced"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hermetic(t)
			t.Setenv("NETWORK_MEMORY", tc.on)
			t.Setenv("CLAUDE_REMOTE_HOST", tc.hosts)
			if err := memory.SaveSync(st); err != nil {
				t.Fatal(err)
			}
			if got := memorySyncRows(); !slices.Equal(got, tc.want) {
				t.Fatalf("rows = %+v, want %+v", got, tc.want)
			}
		})
	}
}
