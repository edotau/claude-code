package configure

import (
	"bufio"
	"bytes"
	"github.com/edotau/claude-code/internal/vault"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// runProfile drives agent's Profile row picker with answer and returns the dialog output.
func runProfile(t *testing.T, reg *providers.Registry, agent, answer string) string {
	t.Helper()
	in := strings.NewReader(answer)
	var out bytes.Buffer
	d := &dialog{reg: reg, o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	agentPinRows(d, agent)[0].run()
	return out.String()
}

// The merged Profile row lists off first then the own login; picking writes the record's Profile and keeps its model.
func TestProfileRowPicksOffAndOwnLogin(t *testing.T) {
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "claude", "profile", "") // menuTestEnv's legacy pin would keep re-seeding own logins
	putRec(t, "codex", "MODEL", "keep-me")          // a stored record: a pick must change Profile only
	out := runProfile(t, reg, "codex", "2\n")       // 1) off  2) chatgpt
	if !strings.Contains(out, "1) off") || !strings.Contains(out, "2) chatgpt") {
		t.Errorf("picker = %q, want off then the own login chatgpt", out)
	}
	if r := vault.GetAgentRecord("codex"); r.Profile != "chatgpt" || r.Model != "keep-me" {
		t.Errorf("record after own-login pick = %+v, want profile chatgpt, model kept", r)
	}
	runProfile(t, reg, "codex", "1\n")
	if r := vault.GetAgentRecord("codex"); r.Profile != "" || r.Model != "keep-me" {
		t.Errorf("record after off = %+v, want profile off, model kept", r)
	}
}

// opencode's menu lists its own login zen-free right after off, which the old Route row omitted.
func TestProfileRowOpencodeListsZenFree(t *testing.T) {
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "claude", "profile", "") // menuTestEnv's legacy pin would keep re-seeding own logins
	out := runProfile(t, reg, "opencode", "2\n")
	if !strings.Contains(out, "2) "+opencode.OwnLogin) {
		t.Errorf("picker = %q, want zen-free as row 2", out)
	}
	if got := vault.GetAgentRecord("opencode").Profile; got != opencode.OwnLogin {
		t.Errorf("profile = %q, want %q", got, opencode.OwnLogin)
	}
}

// Every agent's Profile row stays one line within 80 columns, claude's reading automatic.
func TestProfileRowFitsEightyColumns(t *testing.T) {
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "claude", "profile", "") // menuTestEnv's legacy pin would keep re-seeding own logins
	d := &dialog{reg: reg, out: &bytes.Buffer{}}
	for _, a := range seedAgents() {
		r := agentPinRows(d, a)[0]
		if w := rowRenderedWidth([]string{r.label}, r.desc); w > 80 || strings.Contains(r.desc, "\n") {
			t.Errorf("%s profile row = %d cols: %q", a, w, r.desc)
		}
	}
	if desc := agentPinRows(d, "claude")[0].desc; !strings.Contains(desc, "automatic") {
		t.Errorf("claude row = %q, want automatic", desc)
	}
}

// A sign-in seeds only an EMPTY profile with the own login; a chosen profile is never overridden.
func TestAgentActiveOnSeedsOnlyEmptyProfile(t *testing.T) {
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "claude", "profile", "")
	d := &dialog{reg: reg, out: &bytes.Buffer{}}
	agentActiveOn(d, "codex")
	if got := vault.GetAgentRecord("codex").Profile; got != "chatgpt" {
		t.Fatalf("off codex after sign-in = %q, want chatgpt", got)
	}
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Profile: "openai"}); err != nil {
		t.Fatal(err)
	}
	agentActiveOn(d, "codex")
	if got := vault.GetAgentRecord("codex").Profile; got != "openai" {
		t.Errorf("chosen profile overridden: %q, want openai", got)
	}
}
