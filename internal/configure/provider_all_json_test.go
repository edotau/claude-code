package configure

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

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
