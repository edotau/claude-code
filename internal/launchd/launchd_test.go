package launchd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type ctlStub struct {
	calls  []string
	loaded bool
	fail   error
}

func (s *ctlStub) call(args ...string) error {
	s.calls = append(s.calls, strings.Join(args, " "))
	if s.fail != nil {
		return s.fail
	}
	if args[0] == "print" && !s.loaded {
		return errors.New("exit status 113: Could not find service \"" + args[len(args)-1] + "\" in domain")
	}
	return nil
}

func useCtl(t *testing.T, s *ctlStub) {
	t.Helper()
	prev := Ctl
	Ctl = s.call
	t.Cleanup(func() { Ctl = prev })
}

func dreamAgent(plist string, body []byte) Agent {
	return Agent{Label: "com.edotau.claude-code.dream", Plist: plist, Domain: "gui/501", Body: body}
}

// TestInstallSkipsCurrentLoadedPlist: a plist equal to Body that launchd already has is not rewritten.
func TestInstallSkipsCurrentLoadedPlist(t *testing.T) {
	stub := &ctlStub{loaded: true}
	useCtl(t, stub)
	plist := filepath.Join(t.TempDir(), "com.edotau.claude-code.dream.plist")
	body := []byte("<plist>dream</plist>\n")
	if err := os.WriteFile(plist, body, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := install(&out, dreamAgent(plist, body), false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(plist)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("plist changed: %q %v", got, err)
	}
	if len(stub.calls) != 1 || stub.calls[0] != "print gui/501/com.edotau.claude-code.dream" {
		t.Fatalf("Ctl calls = %q, want one print", stub.calls)
	}
	if !strings.Contains(out.String(), "dream agent") || !strings.Contains(out.String(), "current: "+plist) {
		t.Fatalf("output = %q", out.String())
	}
}

// A changed plist is written, then the old job is booted out and the new one bootstrapped, in that order.
func TestInstallWritesThenBootstraps(t *testing.T) {
	stub := &ctlStub{}
	useCtl(t, stub)
	dir := t.TempDir()
	plist := filepath.Join(dir, "com.edotau.claude-code.dream.plist")
	body := []byte("<plist>new</plist>\n")
	prepped := false
	a := dreamAgent(plist, body)
	a.Prep = func() error { prepped = true; return os.MkdirAll(filepath.Join(dir, "run"), 0o755) }

	var out bytes.Buffer
	if err := install(&out, a, false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(plist)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("plist = %q %v, want the new body", got, err)
	}
	want := "bootout gui/501/com.edotau.claude-code.dream\nbootstrap gui/501 " + plist
	if !prepped || strings.Join(stub.calls, "\n") != want {
		t.Fatalf("prep %v, calls %q, want %q", prepped, stub.calls, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "run")); err != nil || !strings.Contains(out.String(), "wrote "+plist) {
		t.Fatalf("prep dir %v, output %q", err, out.String())
	}
}

// An unchanged plist launchd does not have is loaded, not rewritten.
func TestInstallLoadsUnchangedUnloaded(t *testing.T) {
	stub := &ctlStub{}
	useCtl(t, stub)
	plist := filepath.Join(t.TempDir(), "com.edotau.claude-code.clip.plist")
	body := []byte("<plist>same</plist>\n")
	if err := os.WriteFile(plist, body, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := install(&out, dreamAgent(plist, body), false); err != nil {
		t.Fatal(err)
	}
	want := "print gui/501/com.edotau.claude-code.dream\nbootstrap gui/501 " + plist
	if strings.Join(stub.calls, "\n") != want || !strings.Contains(out.String(), "loaded "+plist) {
		t.Fatalf("calls %q, output %q; want print then bootstrap", stub.calls, out.String())
	}
}

// dryRun reports the row and never writes, prepares or calls launchctl.
func TestInstallDryRunWritesNothing(t *testing.T) {
	stub := &ctlStub{}
	useCtl(t, stub)
	plist := filepath.Join(t.TempDir(), "job.plist")
	a := Agent{Label: "solo", Plist: plist, Domain: "gui/501", Body: []byte("x"), Prep: func() error {
		t.Error("dry run prepared directories")
		return nil
	}}
	var out bytes.Buffer
	if err := install(&out, a, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(plist); !os.IsNotExist(err) || len(stub.calls) != 0 {
		t.Fatalf("dry run touched something: stat %v, calls %q", err, stub.calls)
	}
	if !strings.Contains(out.String(), "solo agent") || !strings.Contains(out.String(), "would write "+plist) {
		t.Fatalf("output = %q", out.String())
	}
}

// dryRun of a current but unloaded plist reports the load and does not bootstrap.
func TestInstallDryRunSkipsUnloadedLoad(t *testing.T) {
	stub := &ctlStub{}
	useCtl(t, stub)
	plist := filepath.Join(t.TempDir(), "job.plist")
	body := []byte("<plist>same</plist>\n")
	if err := os.WriteFile(plist, body, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := install(&out, dreamAgent(plist, body), true); err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 1 || !strings.HasPrefix(stub.calls[0], "print ") {
		t.Fatalf("calls = %q, want only print", stub.calls)
	}
	if !strings.Contains(out.String(), "would load "+plist) {
		t.Fatalf("output = %q", out.String())
	}
}

// A launchctl print failure other than "service not found" is returned, not treated as unloaded.
func TestInstallPrintFailureNotBootstrapped(t *testing.T) {
	wedged := errors.New("exit status 1: launchctl timed out")
	stub := &ctlStub{fail: wedged}
	useCtl(t, stub)
	plist := filepath.Join(t.TempDir(), "job.plist")
	body := []byte("<plist>same</plist>\n")
	if err := os.WriteFile(plist, body, 0o644); err != nil {
		t.Fatal(err)
	}
	err := install(&bytes.Buffer{}, dreamAgent(plist, body), false)
	if !errors.Is(err, wedged) || len(stub.calls) != 1 || !strings.HasPrefix(stub.calls[0], "print ") {
		t.Fatalf("err %v, calls %q; want the print error and no bootstrap", err, stub.calls)
	}
}

// A plist that exists but cannot be read is returned as that error, not overwritten.
func TestInstallReadErrorNotRewritten(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("mode 000 does not deny the owner a read")
	}
	stub := &ctlStub{}
	useCtl(t, stub)
	plist := filepath.Join(t.TempDir(), "job.plist")
	if err := os.WriteFile(plist, []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(plist, 0o644) })
	err := install(&bytes.Buffer{}, dreamAgent(plist, []byte("other")), false)
	_ = os.Chmod(plist, 0o644)
	got, rerr := os.ReadFile(plist)
	if err == nil || len(stub.calls) != 0 || rerr != nil || string(got) != "secret" {
		t.Fatalf("err %v, calls %q, file %q %v; want the read error, no launchctl, file kept", err, stub.calls, got, rerr)
	}
}

// A launchctl failure with no output keeps the error without a dangling separator.
func TestCtlEmptyOutputKeepsError(t *testing.T) {
	err := joinCtl(errors.New("exit status 1"), nil)
	if err == nil || strings.HasSuffix(err.Error(), ":") {
		t.Fatalf("joinCtl = %q, want the error unchanged", err)
	}
	if got := joinCtl(errors.New("exit status 113"), []byte("  Could not find service\n")); !strings.Contains(got.Error(), "Could not find service") {
		t.Fatalf("joinCtl output = %q", got)
	}
}

// XMLText keeps & and < out of a plist string body; a plain path is unchanged.
func TestXMLTextEscapes(t *testing.T) {
	got, err := XMLText(`a&b<c>`)
	if err != nil || got != "a&amp;b&lt;c&gt;" {
		t.Fatalf("XMLText = %q %v", got, err)
	}
	if plain, err := XMLText("/tmp/clip.sock"); err != nil || plain != "/tmp/clip.sock" {
		t.Fatalf("plain = %q %v", plain, err)
	}
}
