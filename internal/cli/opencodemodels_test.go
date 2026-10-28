package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// ocMenuEnv isolates the vault and points provider "ocfake" at a TLS fake that serves three endpoints.
func ocMenuEnv(t *testing.T) {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PROVIDER_OCFAKE_PAT", "dapi-oc-secret")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"endpoints":[
{"name":"databricks-glm-5-2","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-kimi-k3","task":"llm/v1/chat","state":{"ready":"READY"}},
{"name":"databricks-gpt-5-5","task":"llm/v1/chat","state":{"ready":"READY"}}]}`))
	}))
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
	err := providers.SetProviderFields("ocfake", map[string]any{"preset": "databricks", "host": srv.URL,
		"auth": map[string]any{"type":"pat"}})
	if err != nil {
		t.Fatal(err)
	}
}

func ocMenu(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return captureOutput(t, func() int { return cmdOpencodeMenu(args) })
}

func menuIDs() []string {
	var ids []string
	for _, m := range providers.OpencodeMenu() {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestOpencodeMenuAddRefusesUnservedAndForce(t *testing.T) {
	ocMenuEnv(t)
	if code, _, stderr := ocMenu(t, "add", "--profile", "ocfake", "bogus-model"); code == 0 || !strings.Contains(stderr, "not served") {
		t.Fatalf("bogus add: code=%d stderr=%q", code, stderr)
	}
	if providers.OpencodeMenuFromVault() {
		t.Fatal("a refused add must not write the vault")
	}
	if code, _, stderr := ocMenu(t, "add", "--profile", "ocfake", "databricks-glm-5-2"); code != 0 {
		t.Fatalf("served add: %d %s", code, stderr)
	}
	menu := providers.OpencodeMenu()
	last := menu[len(menu)-1]
	if last.ID != "glm-5-2" || last.Label != "GLM 5.2" || last.Responses || len(menu) != len(providers.DatabricksOpencodeMenu)+1 {
		t.Errorf("menu tail = %+v (len %d)", last, len(menu))
	}
	if code, _, _ := ocMenu(t, "add", "--profile", "ocfake", "--force", "bogus-model=Bogus"); code != 0 {
		t.Fatal("--force must bypass the workspace check")
	}
	menu = providers.OpencodeMenu()
	if g := menu[len(menu)-1]; g.ID != "bogus-model" || g.Label != "Bogus" {
		t.Errorf("forced entry = %+v", g)
	}
}

func TestOpencodeMenuAddGPTDefaultsResponses(t *testing.T) {
	ocMenuEnv(t)
	if code, _, stderr := ocMenu(t, "add", "--profile", "ocfake", "gpt-5-5"); code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	menu := providers.OpencodeMenu()
	if g := menu[len(menu)-1]; g.ID != "gpt-5-5" || !g.Responses {
		t.Errorf("gpt add = %+v", g)
	}
}

func TestOpencodeMenuRemoveAndReset(t *testing.T) {
	ocMenuEnv(t)
	if code, _, stderr := ocMenu(t, "remove", "kimi-k3"); code != 0 {
		t.Fatalf("remove: %d %s", code, stderr)
	}
	for _, id := range menuIDs() {
		if id == "kimi-k3" {
			t.Fatal("kimi-k3 still in menu")
		}
	}
	if !providers.OpencodeMenuFromVault() {
		t.Error("remove must persist the overlay")
	}
	if code, _, stderr := ocMenu(t, "remove", "kimi-k3"); code == 0 || !strings.Contains(stderr, "not in the menu") {
		t.Errorf("second remove: code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := ocMenu(t, "reset"); code != 0 {
		t.Fatalf("reset: %d %s", code, stderr)
	}
	if providers.OpencodeMenuFromVault() || len(providers.OpencodeMenu()) != len(providers.DatabricksOpencodeMenu) {
		t.Error("reset must delete the key and restore the seed")
	}
}

func TestOpencodeMenuImportDryRunThenApply(t *testing.T) {
	ocMenuEnv(t)
	dir := t.TempDir()
	plain := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(plain, []byte("glm-5-2\ngpt-5-5,Five\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := ocMenu(t, "import", plain)
	if code != 0 || !strings.Contains(stdout, "dry run") || !strings.Contains(stdout, "+ glm-5-2") {
		t.Fatalf("dry run: %d out=%q err=%q", code, stdout, stderr)
	}
	if providers.OpencodeMenuFromVault() {
		t.Fatal("dry run wrote the vault")
	}
	if code, _, stderr := ocMenu(t, "import", "--apply", plain); code != 0 {
		t.Fatalf("apply: %d %s", code, stderr)
	}
	menu := providers.OpencodeMenu()
	if len(menu) != 2 || menu[0].ID != "glm-5-2" || menu[1] != (providers.OpencodeModel{ID: "gpt-5-5", Label: "Five", Responses: true}) {
		t.Errorf("menu after apply = %+v", menu)
	}
	cfg := filepath.Join(dir, "opencode.json")
	doc := `{"provider":{"databricks-qa":{"models":{"databricks-kimi-k3":{"name":"Kimi K3 · qa"}}}}}`
	if err := os.WriteFile(cfg, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := ocMenu(t, "import", "--apply", cfg); code != 0 {
		t.Fatalf("json apply: %d %s", code, stderr)
	}
	if got := menuIDs(); len(got) != 1 || got[0] != "kimi-k3" {
		t.Errorf("import must replace the whole menu, got %v", got)
	}
	if code, _, stderr := ocMenu(t, "import", filepath.Join(dir, "missing")); code == 0 || stderr == "" {
		t.Error("missing file must fail")
	}
}

func TestOpencodeMenuListLive(t *testing.T) {
	ocMenuEnv(t)
	if err := providers.SetOpencodeMenu([]providers.OpencodeModel{{ID: "glm-5-2", Label: "GLM 5.2"}, {ID: "ghost", Label: "Ghost"}}); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := ocMenu(t, "list", "--live", "--profile", "ocfake", "--format", "json")
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if len(rows) != 2 || rows[0]["live"] != "served" || rows[1]["live"] != "missing" || rows[0]["source"] != "vault" {
		t.Errorf("rows = %v", rows)
	}
	if strings.Contains(stdout+stderr, "dapi-oc-secret") {
		t.Error("token leaked")
	}
	_, text, _ := ocMenu(t, "list", "--live", "--profile", "ocfake")
	if !strings.Contains(text, "served") || !strings.Contains(text, "missing") {
		t.Errorf("text list:\n%s", text)
	}
}

func TestOpencodeMenuListDefaultSource(t *testing.T) {
	ocMenuEnv(t)
	code, stdout, _ := ocMenu(t, "list", "--format", "json")
	var rows []map[string]any
	if code != 0 || json.Unmarshal([]byte(stdout), &rows) != nil || len(rows) != len(providers.DatabricksOpencodeMenu) || rows[0]["source"] != "default" {
		t.Errorf("code=%d rows=%v", code, rows)
	}
}

func TestOpencodeMenuHelpWithin80Cols(t *testing.T) {
	ocMenuEnv(t)
	for _, verb := range opencodeMenuVerbs {
		_, stdout, stderr := ocMenu(t, verb, "-h")
		out := stdout + stderr
		if strings.TrimSpace(out) == "" {
			t.Errorf("%s -h printed nothing", verb)
		}
		for _, l := range strings.Split(out, "\n") {
			if n := len([]rune(l)); n > 80 {
				t.Errorf("%s help line %d cols: %q", verb, n, l)
			}
		}
	}
}
