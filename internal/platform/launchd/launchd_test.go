package launchd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
	if err := installSerialized(&out, dreamAgent(plist, body), false); err != nil {
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
	if err := installSerialized(&out, a, false); err != nil {
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
	if err := installSerialized(&out, dreamAgent(plist, body), false); err != nil {
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
	if err := installSerialized(&out, a, true); err != nil {
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
	if err := installSerialized(&out, dreamAgent(plist, body), true); err != nil {
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
	err := installSerialized(&bytes.Buffer{}, dreamAgent(plist, body), false)
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
	err := installSerialized(&bytes.Buffer{}, dreamAgent(plist, []byte("other")), false)
	_ = os.Chmod(plist, 0o644)
	got, rerr := os.ReadFile(plist)
	if err == nil || len(stub.calls) != 0 || rerr != nil || string(got) != "secret" {
		t.Fatalf("err %v, calls %q, file %q %v; want the read error, no launchctl, file kept", err, stub.calls, got, rerr)
	}
}

// A launchctl failure with no output keeps the error without a dangling separator.
func TestCtlEmptyOutputKeepsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell stub")
	}
	dir := t.TempDir()
	stub := "#!/bin/sh\nif [ \"$1\" = noisy ]; then echo '  Could not find service'; fi\nexit 113\n"
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := Ctl("quiet"); err == nil || strings.HasSuffix(err.Error(), ":") {
		t.Fatalf("Ctl = %q, want the error unchanged", err)
	}
	if err := Ctl("noisy"); err == nil || !strings.Contains(err.Error(), "Could not find service") {
		t.Fatalf("Ctl output = %q", err)
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

// ctlExit models launchctl's exit status and diagnostic without invoking launchctl.
type ctlExit struct {
	code    int
	message string
}

func (e ctlExit) Error() string { return e.message }
func (e ctlExit) ExitCode() int { return e.code }

type reloadStub struct {
	plist, loaded, failPhase string
	failure                  error
	calls                    []string
}

func (s *reloadStub) call(args ...string) error {
	s.calls = append(s.calls, args[0])
	if args[0] == s.failPhase {
		return s.failure
	}
	switch args[0] {
	case "print":
		if s.loaded == "" {
			return errors.New("exit status 113: Could not find service")
		}
	case "bootout":
		if s.loaded == "" {
			return ctlExit{3, "exit status 3: Boot-out failed: 3: No such process"}
		}
		s.loaded = ""
	case "bootstrap":
		b, err := os.ReadFile(s.plist)
		if err != nil {
			return err
		}
		s.loaded = string(b)
	}
	return nil
}

func useReloadStub(t *testing.T, s *reloadStub) {
	t.Helper()
	prev := Ctl
	Ctl = s.call
	t.Cleanup(func() { Ctl = prev })
}

func checkReloadRetry(t *testing.T, s *reloadStub, a Agent) {
	t.Helper()
	if got, err := os.ReadFile(a.Plist); err != nil || !bytes.Equal(got, a.Body) {
		t.Fatalf("written plist = %q, %v", got, err)
	}
	if _, err := os.Stat(a.Plist + ".pending"); err != nil {
		t.Fatalf("pending marker missing: %v", err)
	}
	s.failPhase, s.calls = "", nil
	if err := installSerialized(&bytes.Buffer{}, a, false); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if strings.Join(s.calls, ",") != "bootout,bootstrap" || s.loaded != string(a.Body) {
		t.Fatalf("retry calls %q, loaded %q", s.calls, s.loaded)
	}
	if _, err := os.Stat(a.Plist + ".pending"); !os.IsNotExist(err) {
		t.Fatalf("pending marker remains: %v", err)
	}
	s.calls = nil
	if err := installSerialized(&bytes.Buffer{}, a, false); err != nil || strings.Join(s.calls, ",") != "print" {
		t.Fatalf("current install: calls %q, err %v", s.calls, err)
	}
}

func TestInstallRetriesFailedReload(t *testing.T) {
	for _, phase := range []string{"bootout", "bootstrap"} {
		t.Run(phase, func(t *testing.T) {
			plist := filepath.Join(t.TempDir(), "job.plist")
			if err := os.WriteFile(plist, []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}
			s := &reloadStub{plist: plist, loaded: "old", failPhase: phase, failure: errors.New("injected failure")}
			useReloadStub(t, s)
			a := dreamAgent(plist, []byte("new"))
			if err := installSerialized(&bytes.Buffer{}, a, false); !errors.Is(err, s.failure) {
				t.Fatalf("first install error = %v, want %v", err, s.failure)
			}
			if phase == "bootout" && strings.Join(s.calls, ",") != "bootout" {
				t.Fatalf("bootout failure must stop before bootstrap: %q", s.calls)
			}
			checkReloadRetry(t, s, a)
		})
	}
}

func TestInstallRetriesKilledReload(t *testing.T) {
	for _, loaded := range []string{"old", "", "new"} {
		t.Run("loaded="+loaded, func(t *testing.T) {
			plist := filepath.Join(t.TempDir(), "job.plist")
			a := dreamAgent(plist, []byte("new"))
			for _, file := range []string{plist, plist + ".pending"} {
				if err := os.WriteFile(file, a.Body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			s := &reloadStub{plist: plist, loaded: loaded}
			useReloadStub(t, s)
			checkReloadRetry(t, s, a)
		})
	}
}

func TestInstallBootoutNotLoaded(t *testing.T) {
	for _, err := range []error{
		ctlExit{113, "exit status 113: Could not find service \"job\" in domain"},
		ctlExit{3, "exit status 3: Boot-out failed: 3: No such process\nTry re-running as root."},
	} {
		t.Run(err.Error(), func(t *testing.T) {
			plist := filepath.Join(t.TempDir(), "job.plist")
			var calls []string
			prev := Ctl
			Ctl = func(args ...string) error {
				calls = append(calls, args[0])
				if args[0] == "bootout" {
					return err
				}
				return nil
			}
			t.Cleanup(func() { Ctl = prev })
			if got := installSerialized(&bytes.Buffer{}, dreamAgent(plist, []byte("new")), false); got != nil {
				t.Fatal(got)
			}
			if strings.Join(calls, ",") != "bootout,bootstrap" {
				t.Fatalf("calls = %q", calls)
			}
		})
	}
}

func TestBootoutOnlyIgnoresIdentifiedAbsence(t *testing.T) {
	for _, err := range []error{
		errors.New("exit status 3: Boot-out failed: 3: No such process"),
		ctlExit{5, "exit status 5: Boot-out failed: 3: No such process"},
		ctlExit{3, "exit status 3: Boot-out failed: 3: No such process but failed"},
		ctlExit{5, "exit status 5: Input/output error"},
	} {
		if notBootedOut(err) {
			t.Errorf("unexpectedly ignored %v", err)
		}
	}
}

func TestInstallMarkerWriteFailure(t *testing.T) {
	plist := filepath.Join(t.TempDir(), "job.plist")
	if err := os.WriteFile(plist, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Atomic replacement of a nonempty directory is guaranteed to fail.
	if err := os.Mkdir(plist+".pending", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plist+".pending", "child"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stub := &ctlStub{}
	useCtl(t, stub)
	if err := installSerialized(&bytes.Buffer{}, dreamAgent(plist, []byte("new")), false); err == nil {
		t.Fatal("expected marker write failure")
	}
	got, err := os.ReadFile(plist)
	if err != nil || string(got) != "old" || len(stub.calls) != 0 {
		t.Fatalf("plist %q, err %v, calls %q", got, err, stub.calls)
	}
}

func TestInstallDryRunPending(t *testing.T) {
	stub := &ctlStub{loaded: true}
	useCtl(t, stub)
	plist := filepath.Join(t.TempDir(), "job.plist")
	body := []byte("new")
	for _, path := range []string{plist, plist + ".pending"} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := installSerialized(&out, dreamAgent(plist, body), true); err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 0 || !strings.Contains(out.String(), "would write "+plist) {
		t.Fatalf("calls %q, output %q", stub.calls, out.String())
	}
	if got, err := os.ReadFile(plist + ".pending"); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("pending changed: %q, %v", got, err)
	}
}

func TestInstallSerializesReload(t *testing.T) {
	plist := filepath.Join(t.TempDir(), "job.plist")
	entered, release := make(chan struct{}, 2), make(chan struct{})
	prev := Ctl
	Ctl = func(args ...string) error {
		if args[0] == "bootout" {
			entered <- struct{}{}
			<-release
		}
		return nil
	}
	t.Cleanup(func() { Ctl = prev })
	a := dreamAgent(plist, []byte("new"))
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- installSerialized(&bytes.Buffer{}, a, false) }()
	<-entered
	go func() { second <- installSerialized(&bytes.Buffer{}, a, false) }()
	select {
	case <-entered:
		close(release)
		<-first
		<-second
		t.Fatal("second reload ran before first finished")
	case err := <-second:
		close(release)
		<-first
		t.Fatalf("second install ran before first finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}
