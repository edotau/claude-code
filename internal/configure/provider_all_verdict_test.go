package configure

import (
	"bufio"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

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
