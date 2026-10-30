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
"dev":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net","cliProfile":"dev","auth":{"type":"oauth"}},
"qa":{"preset":"databricks","host":"https://adb-2.azuredatabricks.net","cliProfile":"qa","auth":{"type":"key","command":"echo x"}},
"prod":{"preset":"databricks","host":"https://adb-3.azuredatabricks.net","cliProfile":"prod","auth":{"type":"key","command":"echo x"}}},
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
	for _, k := range []string{providers.PinProfile, providers.PinModel} {
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
	b, _ := json.Marshal(map[string]int64{"schema": 1, "exp": exp.Unix()})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
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
		t.Errorf("env row text = %q / %q", envRows[2][1], envRows[0][1])
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
	if !strings.Contains(out, "qa: no clipboard here; the agents fetch via the proxy") {
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
	if !strings.Contains(dbxOut(d), "no clipboard here; the agents fetch via the proxy") {
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

func TestDbxApplyAllSetsAgentProfilePins(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{}
	installDbxFake(t, f)
	f.picks = []int{4, 0, -1} // Apply to agents -> All agents
	runDatabricksEnv(d, "qa")
	for _, a := range dbxAgents {
		if got := providers.Pin(a.pin); got != "qa" {
			t.Errorf("%s pin %s = %q, want qa", a.name, a.pin, got)
		}
	}
	if !d.changed || !strings.Contains(dbxOut(d), "qa") {
		t.Errorf("changed=%v out=%q", d.changed, dbxOut(d))
	}
	if f.rows[1][0][0] != "All agents" || len(f.rows[1]) != len(dbxAgents)+1 {
		t.Errorf("apply rows = %v", f.rows[1])
	}
	if f.titles[1] != "apply qa to" {
		t.Errorf("apply title = %q", f.titles[1])
	}
}

func TestDbxApplyPerAgentSetsOnePin(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{}
	installDbxFake(t, f)
	f.picks = []int{4, 2, -1} // codex is dbxAgents[1] -> picker row 2
	runDatabricksEnv(d, "qa")
	for _, a := range dbxAgents {
		want := ""
		if a.name == "codex" {
			want = "qa"
		}
		if got := providers.Pin(a.pin); got != want {
			t.Errorf("%s pin = %q, want %q", a.name, got, want)
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

// TestDbxApplyRowsFitEightyColumns: the rows dbxApply really hands the picker stay single-line at 80 columns.
func TestDbxApplyRowsFitEightyColumns(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{}
	installDbxFake(t, f)
	f.picks = []int{4, -1, -1}
	runDatabricksEnv(d, "qa")
	if len(f.rows) < 2 || len(f.rows[1]) != len(dbxAgents)+1 {
		t.Fatalf("apply picker not captured: %v", f.rows)
	}
	var labels []string
	for _, r := range f.rows[1] {
		labels = append(labels, r[0])
	}
	for _, r := range f.rows[1] {
		if w := rowRenderedWidth(labels, r[1]); w > 80 {
			t.Errorf("apply row %q = %d cols", r[0], w)
		}
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
	want := map[string]string{"dev": "dev u2m no login", "prod": "prod command logged in", "qa": "qa command token expires in 59m"}
	for _, l := range lines {
		got := strings.Join(strings.Fields(l), " ")
		f0 := strings.Fields(l)[0]
		if got != want[f0] {
			t.Errorf("line %q = %q, want %q", f0, got, want[f0])
		}
		delete(want, f0)
	}
	if len(want) != 0 {
		t.Errorf("profiles missing from report: %v", want)
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
	if got := dbxOut(d); got != "" {
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

// dbxSetUpEnv is a hermetic dialog over three declared Databricks profiles (dev, qa, prod) in the current overlay shape.
func dbxSetUpEnv(t *testing.T) *dialog {
	t.Helper()
	vaulttest.UseKeyring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, k := range []string{providers.PinProfile, providers.PinModel, providers.PinCodexProfile, providers.PinOpencodeProfile} {
		t.Setenv(k, "")
	}
	row := func(n string) string {
		return `"` + n + `":{"preset":"databricks","host":"https://` + n + `.cloud.databricks.com"}`
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{`+row("dev")+","+row("qa")+","+row("prod")+`}}}`)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return &dialog{reg: reg, out: &bytes.Buffer{}}
}

func TestDbxSetUpOrderAndStops(t *testing.T) {
	loginErr, mintErr := errors.New("browser closed"), errors.New("invalid_grant")
	for _, c := range []struct {
		name          string
		loginErr      error
		mintErr       error
		picks         []int
		calls         []string
		want, notWant string
		changed       bool
	}{
		{"success", nil, nil, []int{0}, []string{"login:dev", "mint:dev"}, "token verified", "stopped", true},
		{"login fails", loginErr, nil, []int{0}, []string{"login:dev"}, "stopped at sign-in: browser closed", "token verified", false},
		{"mint fails", nil, mintErr, []int{0}, []string{"login:dev", "mint:dev"}, "stopped at token check: invalid_grant", "agents", true},
		{"apply cancelled", nil, nil, nil, []string{"login:dev", "mint:dev"}, "signed in; agents were not changed", "stopped", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := dbxSetUpEnv(t)
			f := &dbxFake{picks: c.picks, mintErr: c.mintErr, authType: "u2m", expiry: time.Now().Add(time.Hour)}
			installDbxFake(t, f)
			if c.loginErr != nil {
				dbxLogin = func(context.Context, string, io.Writer) error {
					f.calls = append(f.calls, "login:dev")
					return c.loginErr
				}
			}
			dbxSetUp(d, "dev")
			out := dbxOut(d)
			if strings.Join(f.calls, ",") != strings.Join(c.calls, ",") {
				t.Errorf("calls = %v, want %v", f.calls, c.calls)
			}
			if !strings.Contains(out, c.want) || strings.Contains(out, c.notWant) || strings.Contains(out, dbxTok) {
				t.Errorf("out = %q, want %q without %q or the token", out, c.want, c.notWant)
			}
			if c.name == "success" {
				if len(f.titles) != 1 || f.titles[0] != "apply dev to" || !strings.Contains(out, "dev") {
					t.Errorf("apply picker titles = %v, out = %q", f.titles, out)
				}
			}
			if reached := len(f.titles) > 0; reached != (c.loginErr == nil && c.mintErr == nil) {
				t.Errorf("apply picker reached = %v after %v", reached, f.calls)
			}
			if d.changed != c.changed {
				t.Errorf("changed = %v, want %v", d.changed, c.changed)
			}
		})
	}
}

func TestDbxEnvPageFirstRowIsSetUp(t *testing.T) {
	d := dbxSetUpEnv(t)
	f := &dbxFake{picks: []int{0, 0}, expiry: time.Now().Add(time.Hour)}
	installDbxFake(t, f)
	runDatabricksEnv(d, "dev")
	if got := f.rows[0][0]; got != [2]string{"Set up", "sign in, verify a token, apply to agents"} {
		t.Errorf("first row = %v", got)
	}
	if strings.Join(f.calls, ",") != "login:dev,mint:dev" {
		t.Errorf("calls = %v", f.calls)
	}
}
