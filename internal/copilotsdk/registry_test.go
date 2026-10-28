package copilotsdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	copilot "github.com/github/copilot-sdk/go"
)

// init turns this test binary into a fake copilot CLI when the SDK spawns it with FAKE_COPILOT_CLI=1; it runs
// before TestMain, so a child never reaches the real tests.
func init() {
	if os.Getenv("FAKE_COPILOT_CLI") == "1" {
		fakeCopilotCLI(os.Stdin, os.Stdout)
		os.Exit(0)
	}
}

// fakeCopilotCLI speaks Content-Length framed JSON-RPC: version from FAKE_COPILOT_VERSION, switchTo accepts only
// FAKE_COPILOT_MODELS (comma list), and every call is appended to FAKE_COPILOT_LOG as "method params".
func fakeCopilotCLI(in io.Reader, out io.Writer) {
	r := bufio.NewReader(in)
	known := strings.Split(os.Getenv("FAKE_COPILOT_MODELS"), ",")
	logf, _ := os.OpenFile(os.Getenv("FAKE_COPILOT_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(out, "Content-Length: %d\r\n\r\n%s", len(b), b)
	}
	for {
		n := 0
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if line = strings.TrimSpace(line); line == "" {
				break
			}
			if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
				n, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &req) != nil || req.Method == "" || len(req.ID) == 0 {
			continue // a response or notification: nothing to answer
		}
		fmt.Fprintf(logf, "%s %s\n", req.Method, req.Params)
		var p struct {
			SessionID string `json:"sessionId"`
			ModelID   string `json:"modelId"`
		}
		_ = json.Unmarshal(req.Params, &p)
		res := map[string]any{}
		switch req.Method {
		case "connect":
			res = map[string]any{"ok": true, "protocolVersion": 3, "version": os.Getenv("FAKE_COPILOT_VERSION")}
		case "status.get":
			res = map[string]any{"version": os.Getenv("FAKE_COPILOT_VERSION"), "protocolVersion": 3}
		case "session.create", "session.resume":
			res = map[string]any{"sessionId": cmpOr(p.SessionID, "fake-session")}
		case "session.model.switchTo":
			if !slices.Contains(known, p.ModelID) {
				send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": "unknown model " + p.ModelID}})
				continue
			}
		}
		send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

const registryPick = "dbx-openai/gpt-5-5"

// registryOpts is a valid registry-route Options aimed at the fake CLI (this test binary).
func registryOpts(t *testing.T, version, log string, known ...string) Options {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		CLIPath: exe, Model: registryPick,
		Providers: []copilot.NamedProviderConfig{{Name: "dbx-openai", Type: "openai"}},
		Models:    []copilot.ProviderModelConfig{{ID: "gpt-5-5", Provider: "dbx-openai"}},
		Token:     func() (string, error) { return "tok", nil },
		ChildEnv: []string{"FAKE_COPILOT_CLI=1", "FAKE_COPILOT_VERSION=" + version, "FAKE_COPILOT_LOG=" + log,
			"FAKE_COPILOT_MODELS=" + strings.Join(known, ",")},
	}
}

func openSession(t *testing.T, o Options) (*copilot.Session, error) {
	t.Helper()
	co, err := BuildClientOptions(o)
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
	return newOrResumedSession(ctx, client, o)
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	b, _ := os.ReadFile(log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// A pick outside the registry is refused with ErrUnregisteredPick, before any process starts (Options check) and
// when the CLI itself rejects the switch (so the pick can never silently stay on a GitHub model).
func TestRegistryUnregisteredPick(t *testing.T) {
	o := registryOpts(t, "1.0.104", t.TempDir()+"/log")
	o.Model = "dbx-openai/not-registered"
	if _, err := BuildClientOptions(o); !errors.Is(err, ErrUnregisteredPick) {
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
