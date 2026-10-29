package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const (
	std2Decision = `{"event":"decision","session_id":"s","tool_use_id":"t1","requested_model":"m","matches_router_decision":null,"at":1}` + "\n"
	std2Result   = `{"event":"tool-result","session_id":"s","tool_use_id":"t1","agent_id":"a1","matches_router_decision":null,"at":2}` + "\n"
)

// std2AppendAndJoin appends the child-start to a pre-damaged file and asserts the join still verifies.
func std2AppendAndJoin(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "routing.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	child := codexRouteRecord{Event: "child-start", SessionID: "s", AgentID: "a1", Model: "m", At: 3}
	if err := appendCodexRoute(path, child); err != nil {
		t.Fatalf("appendCodexRoute on damaged file: %v", err)
	}
	rows, err := readCodexRoutes(path)
	if err != nil {
		t.Fatalf("readCodexRoutes after append: %v", err)
	}
	var verified, children int
	for _, r := range rows {
		switch r.Event {
		case "verified":
			verified++
			if r.Matches == nil || !*r.Matches {
				t.Errorf("verified row = %+v, want matches", r)
			}
		case "child-start":
			children++
		}
	}
	if verified != 1 || children != 1 {
		t.Fatalf("verified=%d child-start=%d, want 1 and 1; rows=%+v", verified, children, rows)
	}
}

func TestStd2CodexRoutesTruncatedLastLine(t *testing.T) {
	std2AppendAndJoin(t, std2Decision+std2Result+`{"event":"child-sta`)
}

func TestStd2CodexRoutesMalformedMiddleLine(t *testing.T) {
	std2AppendAndJoin(t, std2Decision+"{not json}\n"+std2Result)
}

func TestStd2CodexRoutesLongLine(t *testing.T) {
	long := `{"event":"note","session_id":"s","matches_router_decision":null,"signal":"` + strings.Repeat("x", 100*1024) + `","at":1}` + "\n"
	std2AppendAndJoin(t, std2Decision+long+std2Result)
}

func TestStd2CodexSpawnPinnedEnvVsVault(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv(providers.PinCodexModel, "gpt-env")
	t.Setenv(providers.PinCodexEffort, "")
	in := ParseInput(strings.NewReader(`{"session_id":"s","tool_input":{"message":"x"}}`))
	if codexSpawnPinned(in) {
		t.Error("process-env CODEX_MODEL with no vault pin vetoed routing, want proceed")
	}
	vaulttest.Put(t, vault.Pins, providers.PinCodexModel, "gpt-vault")
	if !codexSpawnPinned(in) {
		t.Error("vault CODEX_MODEL pin did not veto routing")
	}
}
