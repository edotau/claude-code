package configure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const dbxTok = "dapiSECRETTOKEN0123456789"

const dbxDoc = `{"providers":{
"dev":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net","cliProfile":"dev","auth":{"type":"bearer","authType":"u2m"}},
"qa":{"preset":"databricks","host":"https://adb-2.azuredatabricks.net","cliProfile":"qa","auth":{"type":"bearer","command":"echo x"}},
"prod":{"preset":"databricks","host":"https://adb-3.azuredatabricks.net","cliProfile":"prod","auth":{"type":"bearer","command":"echo x"}}},
"profiles":{"active":"DEFAULT","sets":{"DEFAULT":{
"session":{"account":"dev"},"codex":{"account":"dev"},"copilot":{"account":"dev"},"opencode":{"account":"dev"}}}}}`

type dbxFake struct {
	picks    []int // popped per dbxPick call; exhausted = Back
	titles   []string
	rows     [][][2]string
	calls    []string
	mintErr  error
	copyErr  error
	copied   []string
	sched    []time.Duration
	secret   string
	expiry   time.Time
	authType string
}

func installDbxFake(t *testing.T, f *dbxFake) {
	t.Helper()
	l, m, sp, pk, sc, cp, sh := dbxLogin, dbxMint, dbxSetPAT, dbxPick, dbxSecret, dbxCopy, dbxSchedule
	t.Cleanup(func() {
		dbxLogin, dbxMint, dbxSetPAT, dbxPick, dbxSecret, dbxCopy, dbxSchedule = l, m, sp, pk, sc, cp, sh
	})
	dbxLogin = func(_ context.Context, name string, _ io.Writer) error {
		f.calls = append(f.calls, "login:"+name)
		return nil
	}
	dbxMint = func(_ context.Context, name string) (databricks.MintResult, string, error) {
		f.calls = append(f.calls, "mint:"+name)
		if f.mintErr != nil {
			return databricks.MintResult{}, "", f.mintErr
		}
		at := f.authType
		if at == "" {
			at = "u2m"
		}
		return databricks.MintResult{Provider: name, AuthType: at, Expiry: f.expiry, Len: len(dbxTok)}, dbxTok, nil
	}
	dbxSetPAT = func(_ context.Context, name, v string) (string, error) {
		f.calls = append(f.calls, "pat:"+name+":"+v)
		return "probe ok", nil
	}
	dbxPick = func(title string, rows [][2]string, _ int) (int, error) {
		f.titles = append(f.titles, title)
		f.rows = append(f.rows, rows)
		if len(f.picks) == 0 {
			return -1, nil
		}
		i := f.picks[0]
		f.picks = f.picks[1:]
		return i, nil
	}
	dbxSecret = func(string, func(string) string) (string, error) { return f.secret, nil }
	dbxCopy = func(_ context.Context, v string) error {
		f.copied = append(f.copied, v)
		return f.copyErr
	}
	dbxSchedule = func(after time.Duration, _ string) error { f.sched = append(f.sched, after); return nil }
}

func dbxEnv(t *testing.T) *dialog {
	t.Helper()
	vaulttest.UseKeyring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, k := range []string{providers.PinProvider, providers.PinModel} {
		t.Setenv(k, "")
	}
	vaulttest.Put(t, vault.Overlay, "providers", dbxDoc)
	reg, err := providers.Load()
	if err != nil {
		t.Skip(err)
	}
	return &dialog{reg: reg, out: &bytes.Buffer{}}
}

func dbxOut(d *dialog) string { return d.out.(*bytes.Buffer).String() }

func dbxExpiry(t *testing.T, env string, exp time.Time) {
	t.Helper()
	p := filepath.Join(paths.StateDir(), "cache", "token-expiry", env+".json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]int64{"exp": exp.Unix()})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func dbxProfiles(t *testing.T) providers.Profile {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg.Profiles.Sets["DEFAULT"]
}

func TestDbxDur(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{59*time.Minute + 20*time.Second, "59m"}, {30 * time.Second, "1m"}, {119 * time.Minute, "119m"},
		{120 * time.Minute, "2h00m"}, {5*time.Hour + 7*time.Minute, "5h07m"},
	} {
		if got := dbxDur(c.d); got != c.want {
			t.Errorf("dbxDur(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestDbxStateAndHomeRow(t *testing.T) {
	d := dbxEnv(t)
	if at, text, ok := dbxState(d.reg, "dev"); at != "u2m" || text != "no login" || ok {
		t.Errorf("dev = %q %q %v", at, text, ok)
	}
	if at, text, _ := dbxState(d.reg, "qa"); at != "command" || text != "logged in" {
		t.Errorf("qa no expiry = %q %q", at, text)
	}
	dbxExpiry(t, "qa", time.Now().Add(59*time.Minute+30*time.Second))
	if _, text, _ := dbxState(d.reg, "qa"); text != "token expires in 59m" {
		t.Errorf("qa = %q", text)
	}
	dbxExpiry(t, "qa", time.Now().Add(-time.Minute))
	if _, text, _ := dbxState(d.reg, "qa"); text != "token expired" {
		t.Errorf("expired = %q", text)
	}
	dbxExpiry(t, "qa", time.Now().Add(59*time.Minute+30*time.Second))
	row := databricksRow(d)
	if row.name != "databricks" || row.state != "qa · command" || row.check != "token expires in 59m" {
		t.Errorf("home row = %+v", row)
	}
	if at, text, ok := dbxState(nil, "qa"); at != "" || text != "no login" || ok {
		t.Errorf("nil reg = %q %q %v", at, text, ok)
	}
	if r := databricksRow(&dialog{}); r.state != "not configured" || r.check != "no login" {
		t.Errorf("empty home row = %+v", r)
	}
}

func TestDbxPageRowsAndDispatch(t *testing.T) {
	d := dbxEnv(t)
	dbxExpiry(t, "qa", time.Now().Add(59*time.Minute+30*time.Second))
	f := &dbxFake{expiry: time.Now().Add(59*time.Minute + 30*time.Second)}
	installDbxFake(t, f)
	f.secret = dbxTok

	// env list: qa -> Login, Paste PAT, Mint now, then Back out of both levels.
	f.picks = []int{1, 0, 1, 2, -1, -1}
	runDatabricksPage(d)
	want := []string{"login:qa", "pat:qa:" + dbxTok, "mint:qa"}
	if strings.Join(f.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	if len(f.copied) != 0 {
		t.Errorf("Mint now must not copy: %v", f.copied)
	}
	envRows := f.rows[0]
	if len(envRows) != 3 || envRows[0][0] != "dev" || envRows[1][0] != "qa" || envRows[2][0] != "prod" {
		t.Fatalf("env rows = %v", envRows)
	}
	if envRows[1][1] != "command  token expires in 59m" || envRows[0][1] != "u2m  no login" {
		t.Errorf("env row text = %q / %q", envRows[1][1], envRows[0][1])
	}
	if f.titles[1] != "databricks · qa" {
		t.Errorf("env title = %q", f.titles[1])
	}
	var labels []string
	for _, r := range f.rows[1] {
		labels = append(labels, r[0])
	}
	if strings.Join(labels, "|") != "Login|Paste PAT|Mint now|Copy token|Apply to agents" {
		t.Errorf("actions = %v", labels)
	}
	out := dbxOut(d)
	for _, w := range []string{"qa: login done", "qa: PAT saved · probe ok", "qa · u2m · minted · expires in 59m"} {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, dbxTok) || strings.Contains(out, "dapi") {
		t.Errorf("token leaked into page output:\n%s", out)
	}
}

func TestDbxPasteEmptyIsNoop(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{}
	installDbxFake(t, f)
	f.picks = []int{1, -1}
	runDatabricksEnv(d, "qa")
	if len(f.calls) != 0 {
		t.Errorf("empty paste called %v", f.calls)
	}
}

func TestDbxMintErrorNoTokenInOutput(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{mintErr: errors.New("oidc rejected")}
	installDbxFake(t, f)
	f.picks = []int{2, 3, -1}
	runDatabricksEnv(d, "qa")
	out := dbxOut(d)
	if strings.Count(out, "qa: mint: oidc rejected") != 2 {
		t.Errorf("want two mint errors:\n%s", out)
	}
	if len(f.copied) != 0 {
		t.Errorf("failed mint copied %v", f.copied)
	}
}

func TestDbxCopyReportsClear(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{}
	installDbxFake(t, f)
	f.picks = []int{3, -1}
	runDatabricksEnv(d, "qa")
	out := dbxOut(d)
	if !strings.Contains(out, "qa · copied · clears in 30s") {
		t.Errorf("missing clear note:\n%s", out)
	}
	if len(f.copied) != 1 || f.copied[0] != dbxTok {
		t.Errorf("clipboard got %v", f.copied)
	}
	if len(f.sched) != 1 || f.sched[0] != 30*time.Second {
		t.Errorf("scheduled %v", f.sched)
	}
	if strings.Contains(out, dbxTok) {
		t.Errorf("token leaked:\n%s", out)
	}
}

func TestDbxCopyNoClipboardNotesRouter(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{copyErr: fmt.Errorf("wrapped: %w", clipboard.ErrNoClipboard)}
	installDbxFake(t, f)
	f.picks = []int{3, -1}
	runDatabricksEnv(d, "qa")
	out := dbxOut(d)
	if !strings.Contains(out, "qa: no clipboard here; the agents fetch via the router") {
		t.Errorf("missing router note:\n%s", out)
	}
	if len(f.sched) != 0 {
		t.Errorf("clear scheduled without a copy: %v", f.sched)
	}
	if strings.Contains(out, "clears in") || strings.Contains(out, dbxTok) {
		t.Errorf("bad output:\n%s", out)
	}
}

func TestDbxCopyOtherErrorNoSchedule(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{copyErr: errors.New("pipe broke")}
	installDbxFake(t, f)
	f.picks = []int{3, -1}
	runDatabricksEnv(d, "qa")
	out := dbxOut(d)
	if !strings.Contains(out, "qa: copy: pipe broke") || len(f.sched) != 0 || strings.Contains(out, "via the router") {
		t.Errorf("out=%q sched=%v", out, f.sched)
	}
}

// TestDbxCopyWithRealClipboardStub: the real clipboard.WriteSecret path via a stub xclip; ErrNoClipboard without it.
func TestDbxCopyRealWriteSecretSeam(t *testing.T) {
	defer clipboard.SetWSLForTest(false)()
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", ":0")
	d := dbxEnv(t)
	t.Setenv("PATH", bin)
	f := &dbxFake{}
	installDbxFake(t, f)
	dbxCopy = clipboard.WriteSecret // real writer, no tool on PATH
	f.picks = []int{3, -1}
	runDatabricksEnv(d, "qa")
	if !strings.Contains(dbxOut(d), "no clipboard here; the agents fetch via the router") {
		t.Errorf("real ErrNoClipboard not mapped:\n%s", dbxOut(d))
	}
	sink := filepath.Join(t.TempDir(), "sink")
	stub := "#!/bin/sh\n/bin/cat > " + sink + "\n"
	if err := os.WriteFile(filepath.Join(bin, "xclip"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	f.picks = []int{3, -1}
	runDatabricksEnv(d, "qa")
	got, err := os.ReadFile(sink)
	if err != nil || string(got) != dbxTok {
		t.Errorf("stub clipboard got %q, %v", got, err)
	}
	if strings.Contains(dbxOut(d), dbxTok) {
		t.Errorf("token leaked into output")
	}
}

func TestDbxApplyAllCallsUseAccount(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{}
	installDbxFake(t, f)
	f.picks = []int{4, 0, -1} // Apply to agents -> All four
	runDatabricksEnv(d, "qa")
	set := dbxProfiles(t)
	for _, a := range dbxAgents {
		if set[a].Account != "qa" {
			t.Errorf("%s -> %q, want qa (UseAccount moves every preset row)", a, set[a].Account)
		}
	}
	if !d.changed || !strings.Contains(dbxOut(d), "qa") {
		t.Errorf("changed=%v out=%q", d.changed, dbxOut(d))
	}
	if f.rows[1][0][0] != "All four agents" || len(f.rows[1]) != 5 {
		t.Errorf("apply rows = %v", f.rows[1])
	}
	if f.titles[1] != "apply qa to" {
		t.Errorf("apply title = %q", f.titles[1])
	}
}

func TestDbxApplyPerAgentCallsUseAgentAccount(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{}
	installDbxFake(t, f)
	f.picks = []int{4, 2, -1} // codex is dbxAgents[1] -> picker row 2
	runDatabricksEnv(d, "qa")
	set := dbxProfiles(t)
	if set["codex"].Account != "qa" {
		t.Errorf("codex = %q", set["codex"].Account)
	}
	for _, a := range []string{"session", "copilot", "opencode"} {
		if set[a].Account != "dev" {
			t.Errorf("%s = %q, want untouched dev (UseAgentAccount moves one)", a, set[a].Account)
		}
	}
}

func TestDbxApplyAlreadyAppliedAndCancel(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{}
	installDbxFake(t, f)
	f.picks = []int{4, 2, 4, 2, 4, -1, -1} // codex twice: second is a no-op diff; then cancel the picker
	runDatabricksEnv(d, "qa")
	if !strings.Contains(dbxOut(d), "qa: already applied") {
		t.Errorf("missing already applied:\n%s", dbxOut(d))
	}
}

func TestDbxReportNoTokenAndShape(t *testing.T) {
	d := dbxEnv(t)
	dbxExpiry(t, "qa", time.Now().Add(59*time.Minute+30*time.Second))
	f := &dbxFake{}
	installDbxFake(t, f)
	if !runDatabricks(d) {
		t.Error("report must succeed")
	}
	out := dbxOut(d)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("report lines = %q", lines)
	}
	for i, w := range [][]string{{"dev", "u2m", "no login"}, {"qa", "command", "token expires in 59m"}, {"prod", "command", "logged in"}} {
		got := strings.Join(strings.Fields(lines[i]), " ")
		if got != strings.Join(w, " ") {
			t.Errorf("line %d = %q, want %v", i, got, w)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("report must not mint/login: %v", f.calls)
	}
	if strings.Contains(out, dbxTok) || strings.Contains(out, "dapi") || strings.Contains(out, "eyJ") {
		t.Errorf("token in report:\n%s", out)
	}
}

func TestDbxReportEmptyRegistry(t *testing.T) {
	d := &dialog{out: &bytes.Buffer{}}
	if !runDatabricks(d) {
		t.Error("missing logins are not a failed section")
	}
	if got := dbxOut(d); strings.Count(got, "no login") != 3 || !strings.Contains(got, "-") {
		t.Errorf("report = %q", got)
	}
}

// TestDbxLoginFailureReported: a failing login prints the error and does not mark the dialog changed.
func TestDbxLoginFailureReported(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{}
	installDbxFake(t, f)
	dbxLogin = func(context.Context, string, io.Writer) error { return errors.New("browser closed") }
	f.picks = []int{0, -1}
	runDatabricksEnv(d, "qa")
	if !strings.Contains(dbxOut(d), "qa: login: browser closed") || d.changed {
		t.Errorf("out=%q changed=%v", dbxOut(d), d.changed)
	}
}
