package configure

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
