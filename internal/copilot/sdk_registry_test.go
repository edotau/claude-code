package copilot

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	copilot "github.com/github/copilot-sdk/go"
)

// buildFakeCLI compiles the hermetic fake copilot CLI once per test into a temp dir.
func buildFakeCLI(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "fake")
	if out, err := exec.Command("go", "build", "-o", exe, "./testdata/fakecli").CombinedOutput(); err != nil {
		t.Fatalf("build fakecli: %v\n%s", err, out)
	}
	return exe
}

const registryPick = "dbx-openai/gpt-5-5"

// registryOpts is a valid registry-route SDKOptions aimed at the fake CLI.
func registryOpts(t *testing.T, version, log string, known ...string) SDKOptions {
	t.Helper()
	return SDKOptions{
		CLIPath: buildFakeCLI(t), Model: registryPick,
		Providers: []NamedProvider{{Name: "dbx-openai", Type: "openai"}},
		Models:    []RegistryModel{{ID: "gpt-5-5", Provider: "dbx-openai"}},
		Token:     func() (string, error) { return "tok", nil },
		ChildEnv: []string{"FAKECLI_VERSION=" + version, "FAKECLI_RECORD=" + log,
			"FAKECLI_MODELS=" + strings.Join(known, ",")},
	}
}

func openSession(t *testing.T, o SDKOptions) (*copilot.Session, error) {
	t.Helper()
	co, err := BuildClientOptions(context.Background(), o)
	if err != nil {
		return nil, err
	}
	client := copilot.NewClient(co)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	if err := client.Start(ctx); err != nil {
		t.Fatalf("fake CLI did not start: %v", err)
	}
	t.Cleanup(func() { _ = client.Stop() })
	return newOrResumedSession(ctx, client, o, nil)
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	b, _ := os.ReadFile(log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// A pick outside the registry is refused with ErrUnregisteredPick, before any process starts (SDKOptions check) and
// when the CLI itself rejects the switch (so the pick can never silently stay on a GitHub model).
func TestRegistryUnregisteredPick(t *testing.T) {
	o := registryOpts(t, "1.0.104", t.TempDir()+"/log")
	o.Model = "dbx-openai/not-registered"
	if _, err := BuildClientOptions(context.Background(), o); !errors.Is(err, ErrUnregisteredPick) {
		t.Fatalf("BuildClientOptions err = %v, want ErrUnregisteredPick", err)
	}

	log := t.TempDir() + "/log"
	_, err := openSession(t, registryOpts(t, "1.0.104", log /* the CLI knows no model */))
	if !errors.Is(err, ErrUnregisteredPick) {
		t.Fatalf("session err = %v, want ErrUnregisteredPick from the CLI's rejection", err)
	}
}

// A resumed session is re-pinned through SetModel before any turn (it may carry another wire's saved model).
func TestRegistryResumeRepins(t *testing.T) {
	log := t.TempDir() + "/log"
	o := registryOpts(t, "1.0.104", log, registryPick)
	o.SessionID = "sess-1"
	s, err := openSession(t, o)
	if err != nil {
		t.Fatal(err)
	}
	_ = s
	got := calls(t, log)
	var resume, switched = -1, -1
	for i, c := range got {
		switch {
		case strings.HasPrefix(c, "session.resume "):
			resume = i
		case strings.HasPrefix(c, "session.model.switchTo ") && strings.Contains(c, `"modelId":"`+registryPick+`"`):
			switched = i
		}
	}
	if resume < 0 || switched < resume {
		t.Errorf("calls = %q; want session.resume followed by switchTo %s", got, registryPick)
	}
}

// A CLI older than MinMultiProviderCLI is refused with an explicit error before any session is made.
func TestRegistryProbeBelowMinimum(t *testing.T) {
	log := t.TempDir() + "/log"
	_, err := openSession(t, registryOpts(t, "1.0.103", log, registryPick))
	if !errors.Is(err, ErrNoMultiProvider) || !strings.Contains(err.Error(), MinMultiProviderCLI) {
		t.Fatalf("err = %v, want ErrNoMultiProvider naming %s", err, MinMultiProviderCLI)
	}
	for _, c := range calls(t, log) {
		if strings.HasPrefix(c, "session.create") || strings.HasPrefix(c, "session.resume") {
			t.Errorf("a session was made on an unsupported CLI: %q", c)
		}
	}
	if _, err := openSession(t, registryOpts(t, "1.0.104", t.TempDir()+"/log", registryPick)); err != nil {
		t.Errorf("exactly the minimum was refused: %v", err)
	}
}

// The fake CLI answers a send with an assistant message and records the session config it was created with.
func TestFakeCLISendEmitsReply(t *testing.T) {
	log := t.TempDir() + "/log"
	s, err := openSession(t, registryOpts(t, "1.0.104", log, registryPick))
	if err != nil {
		t.Fatal(err)
	}
	idle := make(chan struct{})
	var reply string
	s.On(func(ev copilot.SessionEvent) {
		switch d := ev.Data.(type) {
		case *copilot.AssistantMessageData:
			reply = d.Content
		case *copilot.SessionIdleData:
			close(idle)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.Send(ctx, copilot.MessageOptions{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-idle:
	case <-ctx.Done():
		t.Fatal("no idle event")
	}
	if reply != "fake reply" {
		t.Errorf("reply = %q, want %q", reply, "fake reply")
	}
	if got := strings.Join(calls(t, log), "\n"); !strings.Contains(got, "session.create ") {
		t.Errorf("record = %q, want session.create params", got)
	}
}
