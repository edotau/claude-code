package configure

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

type fakeSignin struct {
	p     *providers.Provider
	check func() error
	login func(string)
}

func (f fakeSignin) Check(context.Context) (providers.AuthVerdict, string) {
	if f.check == nil {
		return providers.VerdictOK, ""
	}
	err := f.check()
	if err != nil && !errors.Is(err, providers.ErrNeedsLogin) {
		return providers.VerdictTransient, err.Error()
	}
	return providers.Verdict(err), ""
}

func (f fakeSignin) Login(context.Context, io.Reader, io.Writer) error {
	f.login(f.p.Name)
	return nil
}

// allSeams stubs the sign-in factory and TTY gates; per maps row -> check result, tty sets both gates.
func allSeams(t *testing.T, per map[string]func() error, tty bool, timeout time.Duration) *[]string {
	t.Helper()
	f, to, si, se := allSigninFor, allCheckTimeout, allStdinTTY, allStderrTTY
	t.Cleanup(func() { allSigninFor, allCheckTimeout, allStdinTTY, allStderrTTY = f, to, si, se })
	var mu sync.Mutex
	var logins []string
	allSigninFor = func(p *providers.Provider) agent.Signin {
		return fakeSignin{p: p, check: per[p.Name], login: func(n string) {
			mu.Lock()
			defer mu.Unlock()
			logins = append(logins, n)
		}}
	}
	allCheckTimeout = timeout
	allStdinTTY = func() bool { return tty }
	allStderrTTY = func() bool { return tty }
	return &logins
}

func mixed() map[string]func() error {
	return map[string]func() error{
		"dev":  func() error { return nil },
		"qa":   func() error { return fmt.Errorf("wrap: %w", providers.ErrNeedsLogin) },
		"prod": func() error { time.Sleep(time.Second); return nil },
	}
}

func TestAllRowsSorted(t *testing.T) {
	d := dbxEnv(t)
	if got := strings.Join(allRows(d.reg, ""), ","); !strings.Contains(got, "dev,") || !strings.Contains(got, "prod,") {
		t.Fatalf("rows = %q", got)
	}
	if got := allRows(d.reg, "qa"); len(got) != 1 || got[0] != "qa" {
		t.Fatalf("profile rows = %q", got)
	}
	if len(allRows(nil, "")) != 0 {
		t.Fatal("nil registry must yield no rows")
	}
}

func TestAllAllStatuses(t *testing.T) {
	cases := []struct {
		name string
		f    func() error
		want string
	}{
		{"ok", func() error { return nil }, allOK},
		{"needs login", func() error { return providers.ErrNeedsLogin }, allNeedsLogin},
		{"timeout", func() error { time.Sleep(time.Second); return nil }, allTimedOut},
		{"error", func() error { return errors.New("boom") }, allError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := dbxEnv(t)
			allSeams(t, map[string]func() error{"dev": c.f}, false, 50*time.Millisecond)
			got := allCheckAll(context.Background(), d.reg, []string{"dev"})
			if got[0].Status != c.want || got[0].Row != "dev" {
				t.Fatalf("got %+v, want status %q", got[0], c.want)
			}
			if c.want == allTimedOut && got[0].Status == allNeedsLogin {
				t.Fatal("timeout must never be needs login")
			}
		})
	}
}

func TestAllAllMixedReportAndExit(t *testing.T) {
	for _, check := range []bool{false, true} {
		d := dbxEnv(t)
		logins := allSeams(t, mixed(), false, 50*time.Millisecond)
		d.o.All, d.o.Check = true, check
		if runProviderAll(d) {
			t.Fatalf("check=%v: mixed rows must not be all-ok", check)
		}
		out := dbxOut(d)
		for _, w := range []string{"dev: ok", "qa: needs login", "prod: timed out"} {
			if !strings.Contains(out, w) {
				t.Errorf("check=%v: output missing %q:\n%s", check, w, out)
			}
		}
		if len(*logins) != 0 {
			t.Errorf("check=%v: login attempted without a TTY: %v", check, *logins)
		}
	}
}

func TestAllAllAllOKTrue(t *testing.T) {
	d := dbxEnv(t)
	allSeams(t, nil, false, time.Second)
	d.o.All = true
	if !runProviderAll(d) {
		t.Fatalf("all ok must be true:\n%s", dbxOut(d))
	}
}

func TestAllAllCheckExitCode(t *testing.T) {
	d := dbxEnv(t)
	allSeams(t, mixed(), true, 50*time.Millisecond)
	d.o.All, d.o.Check = true, true
	if runProviderAll(d) {
		t.Fatal("--check with a needs-login row must fail")
	}
}

func TestAllAllLoginGate(t *testing.T) {
	cases := []struct {
		name          string
		tty, chk, dry bool
		wantLogin     bool
	}{
		{"tty", true, false, false, true},
		{"no tty", false, false, false, false},
		{"check", true, true, false, false},
		{"dry run", true, false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := dbxEnv(t)
			logins := allSeams(t, map[string]func() error{"qa": func() error { return providers.ErrNeedsLogin }}, c.tty, time.Second)
			d.o.All, d.o.Check, d.o.DryRun = true, c.chk, c.dry
			d.br = bufio.NewReader(strings.NewReader("y\n"))
			runProviderAll(d)
			if got := len(*logins) > 0; got != c.wantLogin {
				t.Fatalf("login attempted=%v want %v (%v)", got, c.wantLogin, *logins)
			}
			if c.wantLogin && (*logins)[0] != "qa" {
				t.Fatalf("logged into %v", *logins)
			}
		})
	}
}

func TestAllAllLoginDeclined(t *testing.T) {
	d := dbxEnv(t)
	logins := allSeams(t, map[string]func() error{"qa": func() error { return providers.ErrNeedsLogin }}, true, time.Second)
	d.o.All = true
	d.br = bufio.NewReader(strings.NewReader("n\n"))
	if runProviderAll(d) || len(*logins) != 0 {
		t.Fatalf("declined login must stay failing with no login: %v", *logins)
	}
}

func TestAllAllJSON(t *testing.T) {
	d := dbxEnv(t)
	allSeams(t, mixed(), false, 50*time.Millisecond)
	d.o.All, d.o.JSON, d.o.Check = true, true, true
	runProviderAll(d)
	var got []AllRowStatus
	if err := json.Unmarshal([]byte(dbxOut(d)), &got); err != nil {
		t.Fatalf("output is not pure JSON: %v\n%s", err, dbxOut(d))
	}
	want := map[string]string{"dev": allOK, "qa": allNeedsLogin, "prod": allTimedOut}
	for _, s := range got {
		if _, ok := want[s.Row]; !ok {
			continue
		}
		if want[s.Row] != s.Status {
			t.Errorf("%s = %q want %q", s.Row, s.Status, want[s.Row])
		}
	}
}

func TestAllLine(t *testing.T) {
	for _, c := range []struct {
		s    AllRowStatus
		want string
	}{
		{AllRowStatus{"dev", allOK, "expires in 5m"}, "dev: ok (expires in 5m)"},
		{AllRowStatus{"dev", allOK, ""}, "dev: ok"},
		{AllRowStatus{"qa", allNeedsLogin, ""}, "qa: needs login"},
		{AllRowStatus{"qa", allError, "boom"}, "qa: error: boom"},
	} {
		if got := allLine(c.s); got != c.want {
			t.Errorf("got %q want %q", got, c.want)
		}
	}
}

// captureStderr swaps os.Stderr for a pipe while fn runs.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	func() {
		defer func() { os.Stderr = old; _ = w.Close() }()
		fn()
	}()
	return <-done
}

// outSignin is a needs-login sign-in whose Login writes a marker to the writer it is handed.
type outSignin struct {
	p      *providers.Provider
	logins *int
}

func (s outSignin) Check(context.Context) (providers.AuthVerdict, string) {
	return providers.VerdictNeedsLogin, ""
}

func (s outSignin) Login(_ context.Context, _ io.Reader, out io.Writer) error {
	*s.logins++
	_, _ = io.WriteString(out, "LOGIN-OUTPUT-MARKER\n")
	return nil
}

func outSeams(t *testing.T, logins *int) {
	t.Helper()
	f, si, se := allSigninFor, allStdinTTY, allStderrTTY
	t.Cleanup(func() { allSigninFor, allStdinTTY, allStderrTTY = f, si, se })
	allSigninFor = func(p *providers.Provider) agent.Signin { return outSignin{p, logins} }
	allStdinTTY, allStderrTTY = func() bool { return true }, func() bool { return true }
}

// M1: --format json with a TTY never prompts or logs in, and stdout carries only the JSON array.
func TestAllJSONSkipsLoginAndStdoutIsPureJSON(t *testing.T) {
	d := dbxEnv(t)
	var logins int
	outSeams(t, &logins)
	d.o.All, d.o.JSON, d.o.Profile = true, true, "qa"
	d.br = bufio.NewReader(strings.NewReader("y\n"))
	stderr := captureStderr(t, func() { runProviderAll(d) })
	if logins != 0 || d.changed {
		t.Fatalf("JSON mode logged in: logins=%d changed=%v", logins, d.changed)
	}
	if strings.Contains(stderr, "Log in now") {
		t.Errorf("JSON mode prompted: %q", stderr)
	}
	var rows []AllRowStatus
	if err := json.Unmarshal([]byte(dbxOut(d)), &rows); err != nil || len(rows) != 1 || rows[0].Status != allNeedsLogin {
		t.Fatalf("stdout not the pure JSON array: %v\n%s", err, dbxOut(d))
	}
}

func TestAllZeroRowsJSONAndText(t *testing.T) {
	d := dbxEnv(t)
	allSeams(t, nil, false, 0)
	d.o.All, d.o.JSON, d.o.Profile = true, true, "no-such-row"
	if !runProviderAll(d) || strings.TrimSpace(dbxOut(d)) != "[]" {
		t.Fatalf("zero rows JSON = %q", dbxOut(d))
	}
	d = dbxEnv(t)
	d.o.All, d.o.Profile = true, "no-such-row"
	runProviderAll(d)
	if !strings.Contains(dbxOut(d), "no providers configured") {
		t.Errorf("zero rows text = %q", dbxOut(d))
	}
}

// M2: the [Y/n] prompt and the sign-in's own output go to stderr, never into the report on d.out.
func TestAllPromptAndLoginOutputGoToStderr(t *testing.T) {
	d := dbxEnv(t)
	var logins int
	outSeams(t, &logins)
	d.o.All, d.o.Profile = true, "qa"
	d.br = bufio.NewReader(strings.NewReader("y\n"))
	stderr := captureStderr(t, func() { runProviderAll(d) })
	if logins != 1 {
		t.Fatalf("logins = %d", logins)
	}
	if !strings.Contains(stderr, "Log in now? [Y/n]") || !strings.Contains(stderr, "LOGIN-OUTPUT-MARKER") {
		t.Errorf("stderr lacks prompt/login output: %q", stderr)
	}
	if out := dbxOut(d); strings.Contains(out, "Log in now") || strings.Contains(out, "LOGIN-OUTPUT-MARKER") {
		t.Errorf("prompt/login output leaked to stdout report:\n%s", out)
	}
}

// verdictSignin reports a fixed verdict per row and records Check and Login calls.
type verdictSignin struct {
	p      *providers.Provider
	v      providers.AuthVerdict
	mu     *sync.Mutex
	checks *[]string
	logins *[]string
}

func (s verdictSignin) Check(context.Context) (providers.AuthVerdict, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.checks = append(*s.checks, s.p.Name)
	return s.v, ""
}

func (s verdictSignin) Login(context.Context, io.Reader, io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	*s.logins = append(*s.logins, s.p.Name)
	return nil
}

func verdictSeams(t *testing.T, vs map[string]providers.AuthVerdict, tty bool) (checks, logins *[]string) {
	t.Helper()
	f, to, si, se := allSigninFor, allCheckTimeout, allStdinTTY, allStderrTTY
	t.Cleanup(func() { allSigninFor, allCheckTimeout, allStdinTTY, allStderrTTY = f, to, si, se })
	var mu sync.Mutex
	checks, logins = &[]string{}, &[]string{}
	allSigninFor = func(p *providers.Provider) agent.Signin {
		return verdictSignin{p: p, v: vs[p.Name], mu: &mu, checks: checks, logins: logins}
	}
	allCheckTimeout = time.Second
	allStdinTTY, allStderrTTY = func() bool { return tty }, func() bool { return tty }
	return checks, logins
}

func TestAllVerdictMappingAndLoginOffer(t *testing.T) {
	for _, c := range []struct {
		v         providers.AuthVerdict
		status    string
		wantLogin bool
	}{
		{providers.VerdictOK, allOK, false},
		{providers.VerdictNeedsLogin, allNeedsLogin, true},
		{providers.VerdictRejected, allRejected, true},
		{providers.VerdictTransient, allError, false},
	} {
		t.Run(string(c.v), func(t *testing.T) {
			d := dbxEnv(t)
			_, logins := verdictSeams(t, map[string]providers.AuthVerdict{"qa": c.v}, true)
			d.o.All, d.o.Profile = true, "qa"
			d.br = bufio.NewReader(strings.NewReader("y\n"))
			got := allCheckAll(context.Background(), d.reg, []string{"qa"})
			if got[0].Status != c.status {
				t.Fatalf("status = %q, want %q", got[0].Status, c.status)
			}
			ok := runProviderAll(d)
			if (len(*logins) == 1) != c.wantLogin || len(*logins) > 1 {
				t.Fatalf("logins = %v, want login=%v", *logins, c.wantLogin)
			}
			if c.v == providers.VerdictOK && !ok {
				t.Error("an ok row must exit clean")
			}
			if c.v != providers.VerdictOK && ok {
				t.Errorf("%s must not report all-ok", c.v)
			}
		})
	}
}

func TestAllNoLoginWithoutTTYForAnyCondemningVerdict(t *testing.T) {
	for _, v := range []providers.AuthVerdict{providers.VerdictNeedsLogin, providers.VerdictRejected} {
		d := dbxEnv(t)
		_, logins := verdictSeams(t, map[string]providers.AuthVerdict{"qa": v}, false)
		d.o.All, d.o.Profile = true, "qa"
		d.br = bufio.NewReader(strings.NewReader("y\n"))
		runProviderAll(d)
		if len(*logins) != 0 {
			t.Errorf("%s: login on a non-TTY: %v", v, *logins)
		}
	}
}

func TestAllProfileFilterChecksOnlyThatRow(t *testing.T) {
	d := dbxEnv(t)
	checks, _ := verdictSeams(t, map[string]providers.AuthVerdict{"qa": providers.VerdictOK, "dev": providers.VerdictRejected}, false)
	d.o.All, d.o.Profile = true, "qa"
	if !runProviderAll(d) {
		t.Fatalf("only qa (ok) is selected:\n%s", dbxOut(d))
	}
	if len(*checks) != 1 || (*checks)[0] != "qa" {
		t.Fatalf("checked %v, want only qa", *checks)
	}
	if strings.Contains(dbxOut(d), "dev") {
		t.Errorf("filtered row leaked into output:\n%s", dbxOut(d))
	}
}

func TestAllRejectedRowText(t *testing.T) {
	d := dbxEnv(t)
	verdictSeams(t, map[string]providers.AuthVerdict{"qa": providers.VerdictRejected}, false)
	d.o.All, d.o.Profile, d.o.Check = true, "qa", true
	runProviderAll(d)
	if !strings.Contains(dbxOut(d), "qa: rejected") {
		t.Errorf("output:\n%s", dbxOut(d))
	}
}
