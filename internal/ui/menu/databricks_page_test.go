package menu

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const dbxTok = "dapiSECRETTOKEN0123456789"

const dbxDoc = `{"providers":{"databricks":{
"dev":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net","cliProfile":"dev","auth":{"type":"oauth"}},
"qa":{"preset":"databricks","host":"https://adb-2.azuredatabricks.net","cliProfile":"qa","auth":{"type":"key","command":"echo x"}},
"prod":{"preset":"databricks","host":"https://adb-3.azuredatabricks.net","cliProfile":"prod","auth":{"type":"key","command":"echo x"}}}}}`

type dbxFake struct {
	picks    []int // popped per menu; exhausted = Back
	titles   []string
	rows     [][][2]string
	calls    []string
	mintErr  error
	checkErr error
	copyErr  error
	copied   []string
	sched    []time.Duration
	secret   string
	expiry   time.Time
	authType string
}

// dbxScreen is the numbered-prompt terminal: it records each menu the page draws into f.titles and f.rows, and
// answers every prompt from f.picks (0-based; -1 or exhausted = Back).
type dbxScreen struct {
	f   *dbxFake
	buf strings.Builder
}

func (s *dbxScreen) Write(p []byte) (int, error) { return s.buf.Write(p) }

func (s *dbxScreen) Read(p []byte) (int, error) {
	var rows [][2]string
	title := ""
	for ln := range strings.SplitSeq(s.buf.String(), "\n") {
		switch {
		case strings.HasPrefix(ln, "  ") && len(ln) > 25:
			rows = append(rows, [2]string{strings.TrimSpace(ln[5:27]), ln[28:]})
		case ln != "" && !strings.HasPrefix(ln, "pick a number"):
			title = ln
		}
	}
	s.buf.Reset()
	s.f.titles = append(s.f.titles, title)
	s.f.rows = append(s.f.rows, rows)
	if len(s.f.picks) == 0 {
		return 0, io.EOF
	}
	i := s.f.picks[0]
	s.f.picks = s.f.picks[1:]
	if i < 0 {
		return copy(p, "\n"), nil
	}
	return copy(p, fmt.Sprintf("%d\n", i+1)), nil
}

func installDbxFake(t *testing.T, f *dbxFake) {
	t.Helper()
	l, m, sp, sc, cp, sh, shl, ck := dbxLogin, dbxMint, dbxSetPAT, dbxSecret, dbxCopy, dbxSchedule, dbxShell, dbxCheckSP
	t.Cleanup(func() {
		dbxLogin, dbxMint, dbxSetPAT, dbxSecret, dbxCopy, dbxSchedule, dbxShell, dbxCheckSP = l, m, sp, sc, cp, sh, shl, ck
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
	dbxShell = func(*dialog) *shell {
		scr := &dbxScreen{f: f}
		return &shell{Out: scr, In: bufio.NewReader(scr)}
	}
	dbxSecret = func(string, string, func(string) string) (string, error) { return f.secret, nil }
	dbxCheckSP = func(_ context.Context, _, id, secret string) error {
		f.calls = append(f.calls, "check:"+id+":"+secret)
		return f.checkErr
	}
	dbxCopy = func(_ context.Context, v string) error {
		f.copied = append(f.copied, v)
		return f.copyErr
	}
	dbxSchedule = func(after time.Duration, _ string) error { f.sched = append(f.sched, after); return nil }
}

func dbxEnv(t *testing.T) *dialog { return dbxDialog(t, dbxDoc) }

// dbxDialog is a hermetic dialog over the providers overlay doc.
func dbxDialog(t *testing.T, doc string) *dialog {
	t.Helper()
	vaulttest.UseKeyring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(providers.EnvSessionProfile, "")
	vaulttest.Put(t, vault.Overlay, "providers", doc)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
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
	b, _ := json.Marshal(map[string]int64{"schema": 2, "exp": exp.Unix()})
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

func TestDbxState(t *testing.T) {
	d := dbxEnv(t)
	if at, text, ok := dbxState(d.reg, "dev"); at != "oauth" || text != "no login" || ok {
		t.Errorf("dev = %q %q %v", at, text, ok)
	}
	if at, text, _ := dbxState(d.reg, "qa"); at != "command" || text != "logged in" {
		t.Errorf("qa no expiry = %q %q", at, text)
	}
	dbxExpiry(t, "qa", time.Now().Add(59*time.Minute+30*time.Second))
	assertDbxText(t, d, "59m")
	dbxExpiry(t, "qa", time.Now().Add(-time.Minute))
	assertDbxText(t, d, dbxExpired)
	if at, text, ok := dbxState(nil, "qa"); at != "" || text != "no login" || ok {
		t.Errorf("nil reg = %q %q %v", at, text, ok)
	}
}

// assertDbxText requires qa's state text to be want.
func assertDbxText(t *testing.T, d *dialog, want string) {
	t.Helper()
	if _, text, _ := dbxState(d.reg, "qa"); ansi.Strip(text) != want {
		t.Errorf("qa = %q, want %q", text, want)
	}
}

func TestDbxHomeRow(t *testing.T) {
	d := dbxEnv(t)
	dbxExpiry(t, "qa", time.Now().Add(59*time.Minute+30*time.Second))
	row := databricksRow(d)
	if row.Name != "databricks" || row.State != "qa · command" || ansi.Strip(row.Check) != "59m" {
		t.Errorf("home row = %+v", row)
	}
	if r := databricksRow(&dialog{}); r.State != "not configured" || r.Check != "no login" {
		t.Errorf("empty home row = %+v", r)
	}
}

func TestDbxPageRowsAndDispatch(t *testing.T) {
	d := dbxEnv(t)
	dbxExpiry(t, "qa", time.Now().Add(59*time.Minute+30*time.Second))
	f := &dbxFake{expiry: time.Now().Add(59*time.Minute + 30*time.Second)}
	installDbxFake(t, f)
	f.secret = dbxTok

	// env list: qa -> Login, Paste PAT, Token, then Back out of both levels.
	f.picks = []int{1, 1, 2, 3, -1, -1}
	runDatabricksPage(d)
	want := []string{"login:qa", "pat:qa:" + dbxTok, "mint:qa"}
	if strings.Join(f.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	if len(f.copied) != 0 {
		t.Errorf("Token must not copy: %v", f.copied)
	}
	assertDbxEnvRows(t, f)
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

// assertDbxEnvRows checks the env list's rows and title and the qa actions page's labels, as the fake recorded them.
func assertDbxEnvRows(t *testing.T, f *dbxFake) {
	t.Helper()
	envRows := f.rows[0]
	if len(envRows) != 3 || envRows[0][0] != "dev" || envRows[1][0] != "qa" || envRows[2][0] != "prod" {
		t.Fatalf("env rows = %v", envRows)
	}
	if ansi.Strip(envRows[1][1]) != "command  59m" || envRows[0][1] != "oauth    no login" {
		t.Errorf("env row text = %q / %q", envRows[1][1], envRows[0][1])
	}
	if f.titles[1] != "databricks · qa" {
		t.Errorf("env title = %q", f.titles[1])
	}
	var labels []string
	for _, r := range f.rows[1] {
		labels = append(labels, r[0])
	}
	if strings.Join(labels, "|") != "Set up|Login|Paste PAT|Token|Copy token|Apply to agents|Identity" {
		t.Errorf("actions = %v", labels)
	}
}

func TestDbxPasteEmptyIsNoop(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{}
	installDbxFake(t, f)
	f.picks = []int{2, -1}
	runDatabricksEnv(d, "qa")
	if len(f.calls) != 0 {
		t.Errorf("empty paste called %v", f.calls)
	}
}

func TestDbxMintErrorNoTokenInOutput(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{mintErr: errors.New("oidc rejected")}
	installDbxFake(t, f)
	f.picks = []int{3, 4, -1}
	runDatabricksEnv(d, "qa")
	out := dbxOut(d)
	if strings.Count(out, "qa: mint: oidc rejected") != 2 {
		t.Errorf("want two mint errors:\n%s", out)
	}
	if len(f.copied) != 0 {
		t.Errorf("failed mint copied %v", f.copied)
	}
}

func TestDbxCopyOutcomes(t *testing.T) {
	for _, c := range []struct {
		name      string
		copyErr   error
		want      string
		notWant   []string
		wantSched bool
	}{
		{"reports clear", nil, "qa · copied · clears in 30s", nil, true},
		{"no clipboard notes router", fmt.Errorf("wrapped: %w", clipboard.ErrNoClipboard),
			"qa: no clipboard here; the agents fetch via the proxy", []string{"clears in"}, false},
		{"other error no schedule", errors.New("pipe broke"), "qa: copy: pipe broke", []string{"via the router"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := dbxEnv(t)
			f := &dbxFake{copyErr: c.copyErr}
			installDbxFake(t, f)
			f.picks = []int{4, -1}
			runDatabricksEnv(d, "qa")
			out := dbxOut(d)
			if !strings.Contains(out, c.want) || strings.Contains(out, dbxTok) {
				t.Errorf("missing %q or token leaked:\n%s", c.want, out)
			}
			for _, nw := range c.notWant {
				if strings.Contains(out, nw) {
					t.Errorf("output has %q:\n%s", nw, out)
				}
			}
			if c.wantSched {
				if len(f.copied) != 1 || f.copied[0] != dbxTok || len(f.sched) != 1 || f.sched[0] != 30*time.Second {
					t.Errorf("copied=%v sched=%v", f.copied, f.sched)
				}
			} else if len(f.sched) != 0 {
				t.Errorf("clear scheduled without a copy: %v", f.sched)
			}
		})
	}
}

// TestDbxCopyRealWriteSecretSeam: the real clipboard.WriteSecret path via a stub xclip; ErrNoClipboard without it.
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
	f.picks = []int{4, -1}
	runDatabricksEnv(d, "qa")
	if !strings.Contains(dbxOut(d), "no clipboard here; the agents fetch via the proxy") {
		t.Errorf("real ErrNoClipboard not mapped:\n%s", dbxOut(d))
	}
	sink := filepath.Join(t.TempDir(), "sink")
	stub := "#!/bin/sh\n/bin/cat > " + sink + "\n"
	if err := os.WriteFile(filepath.Join(bin, "xclip"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	f.picks = []int{4, -1}
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
	f.picks = []int{5, 0, -1} // Apply to agents -> All agents
	runDatabricksEnv(d, "qa")
	for _, a := range dbxAgents {
		if got := vault.GetAgentRecord(a).Profile; got != "qa" {
			t.Errorf("%s profile = %q, want qa", a, got)
		}
	}
	if !d.changed || !strings.Contains(dbxOut(d), "qa") {
		t.Errorf("changed=%v out=%q", d.changed, dbxOut(d))
	}
	if f.rows[1][0][0] != "All agents" || len(f.rows[1]) != len(dbxAgents)+1 || f.titles[1] != "apply qa to" {
		t.Errorf("apply rows = %v title = %q", f.rows[1], f.titles[1])
	}
}

func TestDbxApplyPerAgentSetsOnePin(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{}
	installDbxFake(t, f)
	f.picks = []int{5, 2, -1} // codex is dbxAgents[1] -> picker row 2
	runDatabricksEnv(d, "qa")
	for _, a := range dbxAgents {
		want := ""
		if a == "codex" {
			want = "qa"
		}
		if got := vault.GetAgentRecord(a).Profile; got != want {
			t.Errorf("%s profile = %q, want %q", a, got, want)
		}
	}
}

func TestDbxApplyAlreadyAppliedAndCancel(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{}
	installDbxFake(t, f)
	f.picks = []int{5, 2, 5, 2, 5, -1, -1} // codex twice: second is a no-op; then cancel the picker
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
	want := map[string]string{"dev": "dev oauth no login", "prod": "prod command logged in", "qa": "qa command 59m"}
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
	f.picks = []int{1, -1}
	runDatabricksEnv(d, "qa")
	if !strings.Contains(dbxOut(d), "qa: login: browser closed") || d.changed {
		t.Errorf("out=%q changed=%v", dbxOut(d), d.changed)
	}
}

// dbxSetUpEnv is a hermetic dialog over three declared Databricks profiles (dev, qa, prod) in the current overlay shape.
func dbxSetUpEnv(t *testing.T) *dialog {
	t.Helper()
	row := func(n string) string {
		return `"` + n + `":{"preset":"databricks","host":"https://` + n + `.cloud.databricks.com"}`
	}
	return dbxDialog(t, `{"providers":{"databricks":{`+row("dev")+","+row("qa")+","+row("prod")+`}}}`)
}

// dbxSetUpCase is one TestDbxSetUpOrderAndStops scenario: the failures injected and the calls and output expected.
type dbxSetUpCase struct {
	name          string
	loginErr      error
	mintErr       error
	picks         []int
	calls         []string
	want, notWant string
	changed       bool
}

func TestDbxSetUpOrderAndStops(t *testing.T) {
	loginErr, mintErr := errors.New("browser closed"), errors.New("invalid_grant")
	for _, c := range []dbxSetUpCase{
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
			assertDbxSetUp(t, d, f, c)
		})
	}
}

// assertDbxSetUp checks the calls, output, apply picker and changed flag of one set-up run against c.
func assertDbxSetUp(t *testing.T, d *dialog, f *dbxFake, c dbxSetUpCase) {
	t.Helper()
	out := dbxOut(d)
	if strings.Join(f.calls, ",") != strings.Join(c.calls, ",") {
		t.Errorf("calls = %v, want %v", f.calls, c.calls)
	}
	if !strings.Contains(out, c.want) || strings.Contains(out, c.notWant) || strings.Contains(out, dbxTok) {
		t.Errorf("out = %q, want %q without %q or the token", out, c.want, c.notWant)
	}
	assertDbxApplyPicker(t, f, c, out)
	if d.changed != c.changed {
		t.Errorf("changed = %v, want %v", d.changed, c.changed)
	}
}

// assertDbxApplyPicker requires the apply picker to be reached only when sign-in and the token check both passed.
func assertDbxApplyPicker(t *testing.T, f *dbxFake, c dbxSetUpCase, out string) {
	t.Helper()
	if c.name == "success" {
		if len(f.titles) != 1 || f.titles[0] != "apply dev to" || !strings.Contains(out, "dev") {
			t.Errorf("apply picker titles = %v, out = %q", f.titles, out)
		}
	}
	if reached := len(f.titles) > 0; reached != (c.loginErr == nil && c.mintErr == nil) {
		t.Errorf("apply picker reached = %v after %v", reached, f.calls)
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

// Given the Identity action with sp, a client id and a secret, one save writes the row and seals the M2M secret
// without printing it.
func TestDbxIdentitySavesRowAndSealedSecret(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{secret: "sp-secret-VALUE"}
	installDbxFake(t, f)
	dbxSecret = func(_, prompt string, _ func(string) string) (string, error) {
		if strings.Contains(prompt, "client id") {
			return "sp-client-1", nil
		}
		return f.secret, nil
	}
	f.picks = []int{6, 2, -1} // Identity -> sp
	runDatabricksEnv(d, "dev")
	p := d.reg.Providers["dev"]
	if p.Identity != providers.IdentitySP || p.Auth.SPClientID != "sp-client-1" {
		t.Errorf("row identity=%q spClientId=%q out=%q", p.Identity, p.Auth.SPClientID, dbxOut(d))
	}
	got, ok, _ := vault.ReadSecret(providers.SecretName(providers.GroupDatabricks, "dev", providers.AuthM2M))
	if !ok || got == "" {
		t.Error("M2M secret not stored")
	}
	if strings.Contains(dbxOut(d), "sp-secret-VALUE") {
		t.Errorf("secret printed: %q", dbxOut(d))
	}
	if err := databricks.SaveIdentity(d.reg, "dev", providers.IdentitySP, "", ""); err == nil {
		t.Error("sp without a client id was accepted")
	}
}

// A failed row write must not leave the new secret paired with the old client id: the previous secret is restored.
func TestDbxSaveIdentityRollsBackSecretOnRowFailure(t *testing.T) {
	name := providers.SecretName(providers.GroupDatabricks, "dev", providers.AuthM2M)
	for _, tc := range []struct {
		label, prev string
	}{{"previous secret restored", "sp-secret-OLD"}, {"no previous secret removed", ""}} {
		t.Run(tc.label, func(t *testing.T) {
			d := dbxEnv(t)
			if tc.prev != "" {
				if err := vault.Store(name, tc.prev); err != nil {
					t.Fatal(err)
				}
			}
			orig := databricks.SetRow
			databricks.SetRow = func(string, map[string]any) error { return errors.New("overlay write failed") }
			t.Cleanup(func() { databricks.SetRow = orig })

			err := databricks.SaveIdentity(d.reg, "dev", providers.IdentitySP, "sp-client-2", "sp-secret-NEW")
			if err == nil || !strings.Contains(err.Error(), "overlay write failed") {
				t.Fatalf("err = %v, want the row-write error", err)
			}
			if strings.Contains(err.Error(), "sp-secret") || strings.Contains(dbxOut(d), "sp-secret") {
				t.Errorf("secret leaked: err=%v out=%q", err, dbxOut(d))
			}
			assertStoredSecret(t, name, tc.prev)
			if d.changed {
				t.Error("dialog marked changed after a failed save")
			}
		})
	}
}

// assertStoredSecret requires the vault secret name to hold want (absent when want is empty).
func assertStoredSecret(t *testing.T, name, want string) {
	t.Helper()
	got, found, err := vault.ReadSecret(name)
	if err != nil {
		t.Fatal(err)
	}
	if found != (want != "") || got != want {
		t.Errorf("stored secret found=%v value=%q, want %q", found, got, want)
	}
}

func dbxIdentityRun(t *testing.T, f *dbxFake, pick int, seed bool) (*dialog, string) {
	t.Helper()
	d := dbxEnv(t)
	if seed {
		vaulttest.Put(t, vault.Overlay, "providers", strings.Replace(dbxDoc,
			`"cliProfile":"dev","auth":{"type":"oauth"}`, `"cliProfile":"dev","auth":{"type":"oauth","spClientId":"2c8d8d24-0000-3e6b"}`, 1))
		if err := vault.Store(providers.SecretName(providers.GroupDatabricks, "dev", providers.AuthM2M), "stored-VALUE"); err != nil {
			t.Fatal(err)
		}
		reg, err := providers.Load()
		if err != nil {
			t.Fatal(err)
		}
		d.reg = reg
	}
	installDbxFake(t, f)
	dbxSecret = func(hint, prompt string, _ func(string) string) (string, error) {
		f.calls = append(f.calls, "prompt:"+hint+"|"+prompt)
		if strings.Contains(prompt, "client id") {
			return "new-client-12345678", nil
		}
		return "new-secret-VALUE", nil
	}
	f.picks = []int{pick}
	dbxIdentity(d, "dev")
	t.Logf("calls=%v\n%s", f.calls, dbxOut(d))
	return d, dbxOut(d)
}

func dbxSavedIdentity(t *testing.T) string {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg.Providers["dev"].Identity
}

func dbxVaultSecret(t *testing.T) string {
	t.Helper()
	v, _, err := vault.ReadSecret(providers.SecretName(providers.GroupDatabricks, "dev", providers.AuthM2M))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Given a stored SP id and secret, picking sp neither prompts nor checks; the kept line shows the short id.
func TestDbxIdentityKeepsStoredPair(t *testing.T) {
	f := &dbxFake{checkErr: errors.New("must not run")}
	_, out := dbxIdentityRun(t, f, 2, true)
	if strings.Join(f.calls, ",") != "" || !strings.Contains(out, "SP 2c8d…3e6b · secret stored — kept") ||
		!strings.Contains(out, "dev: identity sp · SP and secret kept") || dbxSavedIdentity(t) != "sp" {
		t.Errorf("calls=%v out=%q", f.calls, out)
	}
	if strings.Contains(out, "stored-VALUE") || dbxVaultSecret(t) != "stored-VALUE" {
		t.Error("secret printed or changed")
	}
}

// Given a failing check, a new pair writes nothing to the vault and the failure line carries no secret.
func TestDbxIdentityChecksBeforeSaving(t *testing.T) {
	f := &dbxFake{checkErr: errors.New("invalid_client")}
	d, out := dbxIdentityRun(t, f, 2, false)
	if !strings.Contains(out, "SP check failed: invalid_client") || strings.Contains(out, "new-secret-VALUE") {
		t.Errorf("out=%q", out)
	}
	if dbxVaultSecret(t) != "" || dbxSavedIdentity(t) == "sp" || d.changed {
		t.Error("something was saved after a failed check")
	}
}

// Given a passing check, the pair is saved and the confirmation fits 80 columns.
func TestDbxIdentitySavesCheckedPair(t *testing.T) {
	f := &dbxFake{}
	_, out := dbxIdentityRun(t, f, 2, false)
	want := "dev: identity sp · SP new-…5678 · secret saved (DATABRICKS_DEV_M2M)"
	if !strings.Contains(out, want) || dbxVaultSecret(t) != "new-secret-VALUE" {
		t.Errorf("out=%q", out)
	}
	if !strings.Contains(strings.Join(f.calls, ","), "check:new-client-12345678:new-secret-VALUE") {
		t.Errorf("calls=%v", f.calls)
	}
	for l := range strings.SplitSeq(out, "\n") {
		if n := utf8.RuneCountInString(l); n > 80 {
			t.Errorf("%d cols: %q", n, l)
		}
	}
}

// Given a stored pair, the extra row re-prompts, hinting the short id and the stored secret.
func TestDbxIdentityChangeRowPrompts(t *testing.T) {
	f := &dbxFake{}
	_, out := dbxIdentityRun(t, f, 3, true)
	if c := strings.Join(f.calls, ","); !strings.Contains(c, "prompt:empty keeps 2c8d…3e6b|") ||
		!strings.Contains(c, "prompt:empty keeps the stored secret|") || !strings.Contains(out, "secret saved") {
		t.Errorf("calls=%v out=%q", f.calls, out)
	}
}

// A registry reload that fails (corrupt overlay) says so and keeps the previous registry.
func TestDbxReloadFailureKeepsPreviousRegistry(t *testing.T) {
	d := dbxEnv(t)
	prev := d.reg
	vaulttest.Put(t, vault.Overlay, "providers", "{not json")
	d.reload()
	if d.reg != prev {
		t.Error("failed reload replaced the registry")
	}
	if !strings.Contains(dbxOut(d), "registry reload failed, showing the previous state") {
		t.Errorf("out=%q, want the reload-failure line", dbxOut(d))
	}
}

// TestDatabricksRowsFitEightyColumns: every page the env list and its actions draw fits an 80-column panel.
func TestDatabricksRowsFitEightyColumns(t *testing.T) {
	menuTestEnv(t)
	d := dbxEnv(t)
	dbxExpiry(t, "prod", time.Now().Add(23*time.Hour+59*time.Minute))
	f := &dbxFake{}
	installDbxFake(t, f)
	f.picks = []int{1, 4, 0, -1, -1}
	runDatabricksPage(d)
	if len(f.rows) < 2 {
		t.Fatalf("pages drawn = %q, want the env list and an env's actions", f.titles)
	}
	for i, set := range f.rows {
		if len(set) > 0 {
			assertFitsEighty(t, f.titles[i], append(set, panelBack))
		}
	}
}

// dbxAgents, agentMaker and the copilot effort values keep their pinned contents; an agentid rename fails here.
func TestMenuListsTieToAgentid(t *testing.T) {
	if want := []string{"claude", "codex", "opencode", "copilot"}; !slices.Equal(dbxAgents, want) {
		t.Errorf("dbxAgents = %v, want %v", dbxAgents, want)
	}
	wantMaker := map[string]string{"claude": "Anthropic", "codex": "OpenAI", "gemini": "Google", "copilot": "GitHub", "opencode": "opencode"}
	if !maps.Equal(agentMaker, wantMaker) {
		t.Errorf("agentMaker = %v, want %v", agentMaker, wantMaker)
	}
	var got []string
	for _, c := range copilotEffortChoices {
		got = append(got, c[1])
	}
	if want := []string{"low", "medium", "high", "xhigh", "max"}; !slices.Equal(got, want) {
		t.Errorf("copilotEffortChoices values = %v, want %v", got, want)
	}
}

func TestDbxIdentityPromptCancellationKeepsStoredPair(t *testing.T) {
	for _, cancelAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("prompt-%d", cancelAt), func(t *testing.T) {
			d := dbxEnv(t)
			if err := databricks.SaveIdentity(d.reg, "dev", providers.IdentityAuto, "old-client", "stored-VALUE"); err != nil {
				t.Fatal(err)
			}
			d.reload()
			f := &dbxFake{}
			installDbxFake(t, f)
			prompts := 0
			dbxSecret = func(string, string, func(string) string) (string, error) {
				prompts++
				if prompts == cancelAt {
					return "", context.Canceled
				}
				return "replacement-client", nil
			}
			dbxIdentityPrompt(d, "dev", providers.IdentitySP, d.reg.Providers["dev"], "stored-VALUE")
			if prompts != cancelAt || len(f.calls) != 0 || d.changed {
				t.Fatalf("cancel performed work: prompts=%d calls=%v changed=%v", prompts, f.calls, d.changed)
			}
			d.reload()
			if got := d.reg.Providers["dev"]; got.Identity != providers.IdentityAuto || got.Auth.SPClientID != "old-client" {
				t.Errorf("identity=%q client=%q after cancellation", got.Identity, got.Auth.SPClientID)
			}
			assertStoredSecret(t, providers.SecretName(providers.GroupDatabricks, "dev", providers.AuthM2M), "stored-VALUE")
		})
	}
}
