package menu

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	_ "github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/serving"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaultdoc"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// runKeysCheck drives the read-only (--check/piped) keys report; !allOK maps to rc 1.
func runKeysCheck(t *testing.T, o Options) (out bytes.Buffer, rc int) {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatalf("providers.Load: %v", err)
	}
	o.In = o.reader()
	d := &dialog{o: o, reg: reg, br: bufio.NewReader(o.In), out: &out}
	d.header("keys")
	allOK, _ := runKeysChanged(d)
	if !allOK {
		rc = 1
	}
	return out, rc
}

// A piped --check on a box with no keys reports every absent credential, writes nothing, and exits 1.
func TestCheckPipedReportsAbsentKeysAndExitsOne(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	vaulttest.Use(t)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	vaulttest.SetRecord(t, "claude", "profile", "openai")
	vaultBefore, _ := os.ReadFile(filepath.Join(cfg, "state", "vault.db"))
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", t.TempDir())
	out, rc := runKeysCheck(t, Options{Check: true, Home: home, In: strings.NewReader("")})
	if rc != 1 {
		t.Errorf("rc = %d, want 1; output:\n%s", rc, out.String())
	}
	for _, want := range []string{"== keys ==", "ABSENT", "claude-code vault set secrets OPENAI_API_KEY"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Errorf("--check wrote into HOME: %v", ents)
	}
	if after, _ := os.ReadFile(filepath.Join(cfg, "state", "vault.db")); !bytes.Equal(after, vaultBefore) {
		t.Error("--check wrote vault.db")
	}
}

// A vendor whose only resolvable provider is another vendor's passthrough login is ABSENT, not "present".
func TestVendorKeyTargetRejectsForeignPassthrough(t *testing.T) {
	cfg, home := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", home)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PATH", t.TempDir())
	out, _ := runKeysCheck(t, Options{Check: true, Agent: "codex", Home: home, In: strings.NewReader("")})
	if !strings.Contains(out.String(), "codex") || strings.Contains(out.String(), "codex      present (subscription") {
		t.Errorf("codex must not read as present on a subscription session:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "codex login") {
		t.Errorf("codex row should name `codex login` as the fix:\n%s", out.String())
	}
}

// A vendor running on its own stored config.
func TestVendorOnOwnConfigReadsAbsentUnderNewChain(t *testing.T) {
	cfg, home, xdg := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for _, k := range []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
	if err := os.MkdirAll(filepath.Join(xdg, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "opencode", "opencode.jsonc"), []byte(`{"model": "google/gemini-3.8-flash"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, rc := runKeysCheck(t, Options{Check: true, Agent: "opencode", Home: home, In: strings.NewReader("")})
	if rc != 1 || !strings.Contains(out.String(), "opencode") || !strings.Contains(out.String(), "ABSENT") {
		t.Errorf("rc = %d; opencode.jsonc's own model must no longer read present:\n%s", rc, out.String())
	}
	if strings.Contains(out.String(), "google/gemini-3.8-flash") {
		t.Errorf("the retired own-config dispatch must not appear in output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "opencode auth login") {
		t.Errorf("fix command must name `opencode auth login`:\n%s", out.String())
	}
}

// An absent credential offers BOTH the browser login and a pasted key.
func TestResolveKeyOffersLoginAndKeyThenStoresPastedKey(t *testing.T) {
	vaulttest.UseKeyring(t)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PATH", t.TempDir())
	kt := configure.KeyTarget{Label: "openai", EnvVar: "OPENAI_API_KEY", LoginArgv: []string{"openai", "login"}, FixCmd: "x"}
	opts := configure.AuthOptions(kt)
	if len(opts) != 2 || opts[0].Argv == nil || opts[1].Env != "OPENAI_API_KEY" || !strings.Contains(opts[1].Desc, "platform.openai.com") {
		t.Fatalf("authOptions = %+v, want browser login then key with its console URL", opts)
	}
	in := strings.NewReader("n\ny\nn\n  qZjciniiclec \n")
	var out bytes.Buffer
	d := &dialog{o: Options{Interactive: true, In: in, Out: &out}, br: bufio.NewReader(in), out: &out}
	if !resolveKey(d, kt) {
		t.Fatalf("resolveKey = false\n%s", out.String())
	}
	got, src, ok := vault.Get("OPENAI_API_KEY")
	if !ok || src != "vault" || got != "qZjciniiclec" {
		t.Errorf("stored key = (%q, %q, %v)", got, src, ok)
	}
	assertVaultRecordSealed(t, "OPENAI_API_KEY", "qZjciniiclec")
	if strings.Contains(out.String(), "logged in") {
		t.Error("login must not have run after being declined")
	}
	assertContainsAll(t, out.String(), []string{"Browser login — openai login? [Y/n]", "Paste API key — create at platform.openai.com/api-keys"})
}

// discardWriter is an io.Writer that throws away everything written to it.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// assertContainsAll requires every want to appear in the dialog output got.
func assertContainsAll(t *testing.T, got string, wants []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("dialog lacks %q:\n%s", want, got)
		}
	}
}

// assertVaultRecordSealed requires the secret record for name not to contain the plaintext secret.
func assertVaultRecordSealed(t *testing.T, name, secret string) {
	t.Helper()
	if err := vault.View(func(tx *vault.Tx) error {
		if raw, _ := tx.Get(vault.Secrets, name); strings.Contains(raw, secret) {
			t.Error("vault record contains the plaintext secret")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// --check on a present, rotatable credential.
func TestCheckStaysReadOnlyWithRotatableCredential(t *testing.T) {
	cfg, home, codexHome := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("PATH", t.TempDir())
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{"access_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, rc := runKeysCheck(t, Options{Check: true, Agent: "codex", Home: home, In: strings.NewReader("")})
	if rc != 0 {
		t.Errorf("rc = %d, want 0 (codex reads present, nothing to fix):\n%s", rc, out.String())
	}
	if strings.Contains(out.String(), "current credential as-is") || strings.Contains(out.String(), "logged in again") {
		t.Errorf("--check must never prompt to rotate or run a rotate CLI:\n%s", out.String())
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Errorf("--check wrote into HOME: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(cfg, "state", "vault.db")); err == nil {
		t.Error("--check wrote vault.db")
	}
}

// `keys` was removed from Sections.
func TestRunUnknownSectionKeysReturnsTwo(t *testing.T) {
	var out bytes.Buffer
	rc := Run(Options{Check: true, In: strings.NewReader(""), Out: &out}, "keys")
	if rc != 2 {
		t.Errorf("rc = %d, want 2:\n%s", rc, out.String())
	}
	if !strings.Contains(out.String(), "unknown section") {
		t.Errorf("output lacks %q:\n%s", "unknown section", out.String())
	}
}

func TestSecretLineEchoesMaskedPreview(t *testing.T) {
	for _, in := range []string{"qz-mnb-Qwerty!plmoknijbuhvygctfxrdzeswaq\n",
		"TAILSCALE_OAUTH_CLIENT_SECRET=qz-mnb-Qwerty!plmoknijbuhvygctfxrdzeswaq\n"} {
		var out strings.Builder
		d := &dialog{out: &out, br: bufio.NewReader(strings.NewReader(in))}
		if _, err := secretLine(d, "> "); err != nil {
			t.Fatal(err)
		}
		if w := len([]rune(strings.Split(strings.TrimSpace(out.String()), "\n")[1])); w > 80 {
			t.Errorf("preview line is %d columns", w)
		}
		if strings.Contains(out.String(), "buhvygct") || !strings.Contains(out.String(), "(40 chars)") {
			t.Errorf("preview leaks or lacks length: %q", out.String())
		}
	}
}

func TestSecretLineFallbackPreservesInputAndOutput(t *testing.T) {
	for _, in := range []string{"", "\n", "  plain value  \r\n", "NAME=private-value\n", "unterminated-value"} {
		t.Run(strings.TrimSpace(in), func(t *testing.T) {
			d, out := newDialog(in)
			got, err := secretLine(d, "> ")
			want := strings.TrimRight(in, "\r\n")
			if err != nil || got != want {
				t.Fatalf("secretLine = (%q, %v), want (%q, nil)", got, err, want)
			}
			wantOut := "> \n"
			if _, value := configure.ParseAssignment(want); value != "" {
				wantOut += "  pasted  " + pastePreview(want) + "\n"
			}
			if out.String() != wantOut {
				t.Errorf("output = %q, want %q", out.String(), wantOut)
			}
		})
	}
}

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

// servedHarness seeds a hermetic vault with a Databricks row "qa".
func servedHarness(t *testing.T, on bool) *int {
	t.Helper()
	vaulttest.Use(t)
	var n int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/scim/v2/Me") {
			_, _ = w.Write([]byte(`{"id":"u1","active":true}`))
			return
		}
		n++
		_, _ = w.Write([]byte(`{"endpoints":[{"name":"m-a","task":"llm/v1/chat","state":{"ready":"READY"}}]}`))
	}))
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = old; srv.Close() })
	row := `{"routing":{"servedAware":%t},"providers":{"databricks":{"qa":{"preset":"databricks","host":%q,` +
		`"auth":{"type":"pat"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", fmt.Sprintf(row, on, srv.URL))
	vaulttest.Secret(t, "DATABRICKS_QA_PAT", "qZkiibmfkedl")
	return &n
}

func cached(t *testing.T) bool {
	t.Helper()
	_, ok := serving.Load("qa")
	return ok
}

// Run wires the warm: a non-interactive provider pin change with the switch on warms; --dry-run does not.
func TestRunProviderPinChangeWarmsUnlessDryRun(t *testing.T) {
	n := servedHarness(t, true)
	var out bytes.Buffer
	if rc := Run(Options{Provider: "qa", DryRun: true, In: strings.NewReader(""), Out: &out}, "provider"); rc != 0 || *n != 0 {
		t.Fatalf("dry-run: rc=%d requests=%d out=%s", rc, *n, out.String())
	}
	Run(Options{Provider: "qa", Check: true, In: strings.NewReader(""), Out: &out}, "provider")
	if *n != 0 {
		t.Fatalf("--check warmed: %d requests", *n)
	}
	if rc := Run(Options{Provider: "qa", In: strings.NewReader(""), Out: &out}, "provider"); rc != 0 {
		t.Fatalf("rc=%d out=%s", rc, out.String())
	}
	if vault.GetAgentRecord("claude").Route.Profile != "qa" || *n == 0 || !cached(t) {
		t.Fatalf("pin=%q requests=%d cached=%v out=%s", vault.GetAgentRecord("claude").Route.Profile, *n, cached(t), out.String())
	}
}

// The routing page runRoutingMenu shows, with long vault-set values, lays out on an 80-column panel unclipped.
func TestRoutingRowFitsEightyColumns(t *testing.T) {
	worst := providers.Routing{
		Weights: map[string]float64{"price": 0.123456789, "headroom": 0.876543211},
		Opencode: providers.OpencodeRouting{
			Ladder:          &providers.RouteOpencodeSubIDs,
			WorkRepoMarkers: &[]string{"gitlab.example.com", "github.example.net"},
			FreeTiers:       &[]string{"sonnet", "haiku"},
			FreeFirstOutput: "90s",
		},
	}
	orig := pickMenu
	t.Cleanup(func() { pickMenu = orig })
	var rows [][2]string
	pickMenu = func(_ string, secs []tui.Section, trailing [2]string, _ int) (int, error) {
		for _, s := range secs {
			rows = append(rows, s.Rows...)
		}
		rows = append(rows, trailing)
		return -1, nil
	}
	runRoutingMenu(&dialog{out: discardWriter{}}, worst, worst)
	assertFitsEighty(t, "routing page", rows)
}

func TestEditRoutingTextReasksOnInvalid(t *testing.T) {
	var f configure.RoutingField
	for _, m := range configure.Modules() {
		for _, c := range m.Fields() {
			if c.Kind == configure.RoutingNumber {
				f = c
			}
		}
	}
	var out strings.Builder
	d := &dialog{out: &out, br: bufio.NewReader(strings.NewReader("abc\n0.5\n"))}
	got, ok := editRoutingText(d, f, providers.Routing{})
	if !ok || got != "0.5" {
		t.Fatalf("editRoutingText = %q, %v; want 0.5, true (output %q)", got, ok, out.String())
	}
}

// lsRows is the `vault ls` section as cell rows, laid out by the function `vault ls` prints through.
func lsRows(t *testing.T, section string) [][]string {
	t.Helper()
	return lsRowsAfter(t, section, 0)
}

// lsRowsAfter is the section's cell rows in its skip-th blank-line-separated block (0 = the table).
func lsRowsAfter(t *testing.T, section string, skip int) [][]string {
	t.Helper()
	raw, err := vaultdoc.Export()
	if err != nil {
		t.Fatal(err)
	}
	var doc vaultdoc.Doc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	stored, err := vault.ListStored(nil)
	if err != nil {
		t.Fatal(err)
	}
	text := vaultdoc.Text(doc, vaultdoc.Records{Secrets: stored}, []string{section}, 200, false)
	var rows [][]string
	for _, line := range strings.Split(text, "\n")[1:] {
		if strings.TrimSpace(line) == "" {
			if skip--; skip < 0 {
				break
			}
			continue
		}
		if skip > 0 {
			continue
		}
		rows = append(rows, regexp.MustCompile(`\s{2,}`).Split(strings.TrimSpace(line), -1))
	}
	return rows
}

// lineHas reports a line holding want's key and value as whole tokens; want is "key" or "key<TAB>value".
func lineHas(line, want string) bool {
	token := func(s string) bool {
		return s == "" || regexp.MustCompile(`(?:^|\s)`+regexp.QuoteMeta(s)+`(?:\s|$)`).MatchString(line)
	}
	key, val, _ := strings.Cut(want, "\t")
	return token(key) && token(val)
}

// pageMisses lists the wants no line of page holds.
func pageMisses(page string, wants []string) []string {
	var miss []string
	for _, w := range wants {
		if !slices.ContainsFunc(strings.Split(page, "\n"), func(l string) bool { return lineHas(l, w) }) {
			miss = append(miss, w)
		}
	}
	return miss
}

// pageCheck fails unless the page holds every want; the same page minus the first want's lines must fail it.
func pageCheck(t *testing.T, page string, wants []string) {
	t.Helper()
	if len(wants) == 0 {
		t.Fatal("no vault ls rows to check")
	}
	if miss := pageMisses(page, wants); len(miss) != 0 {
		t.Errorf("page lacks vault ls rows %q:\n%s", miss, page)
	}
	var kept []string
	for l := range strings.SplitSeq(page, "\n") {
		if !lineHas(l, wants[0]) {
			kept = append(kept, l)
		}
	}
	if len(pageMisses(strings.Join(kept, "\n"), wants[:1])) == 0 {
		t.Errorf("negative control: dropping the %q row still passed", wants[0])
	}
}

func pageTestEnv(t *testing.T) *providers.Registry {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir())
	vaulttest.Use(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// Every `vault ls routing` row, the fallback mode included, is on the routing page.
func TestRoutingPageShowsVaultLsRows(t *testing.T) {
	pageTestEnv(t)
	vaulttest.Put(t, vault.Overlay, "providers",
		`{"routing":{"weights":{"latency":0.3,"price":0.2,"headroom":0.5},"servedAware":true,"spendCap":{"dayUSD":5.25},"opencode":{"freeTiers":["haiku"],"freeFirstOutput":"45s"}}}`)
	vaulttest.Put(t, vault.Env, "AGENT_ERROR_FALLBACK", "all")
	d, out := newDialog("")
	d.o.Interactive = false
	if !runRouting(d) {
		t.Fatalf("runRouting failed: %s", out)
	}
	var wants []string
	for _, r := range lsRows(t, "routing") {
		if len(r) != 2 {
			t.Fatalf("routing row %q, want key and value", r)
		}
		wants = append(wants, regexp.MustCompile(`\[\d+\]$`).ReplaceAllString(r[0], "")+"\t"+r[1])
	}
	pageCheck(t, out.String(), wants)
}

// Every `vault ls secrets` name is on the secrets page, which also lists the expected absent ones.
func TestSecretsPageShowsVaultLsRows(t *testing.T) {
	reg := pageTestEnv(t)
	vaulttest.Secret(t, "PAGE_ONE_SECRET", "x")
	vaulttest.Secret(t, "PAGE_TWO_SECRET", "x")
	d, out := newDialog("")
	d.reg = reg
	secretsLoop(d)
	var wants []string
	for _, r := range lsRows(t, "secrets") {
		wants = append(wants, r[0])
	}
	if len(wants) != 2 {
		t.Fatalf("vault ls secrets rows = %q, want the two seeded", wants)
	}
	tracked := configure.TrackedSecrets(reg)
	if len(tracked) <= len(wants) {
		t.Fatalf("tracked secrets = %d, want expected absent ones beyond the stored", len(tracked))
	}
	for _, s := range tracked {
		wants = append(wants, s.Name)
	}
	pageCheck(t, out.String(), wants)
}

// Every `vault ls agents` table row is on the agents page, profile included, and each overlay launch.env name too.
func TestAgentsPageShowsVaultLsRows(t *testing.T) {
	reg := pageTestEnv(t)
	vaulttest.SetRecord(t, "codex", "profile", "ultra")
	vaulttest.SetRecord(t, "gemini", "profile", "ultra")
	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"codex":{"launch":{"env":["X_Y=hidden-value","Z_W="]}}}}`)
	d, out := newDialog("")
	d.o.Interactive = false
	d.o.DryRun = true
	d.o.Home = os.Getenv("HOME")
	d.reg = reg
	runAgents(d)
	var wants []string
	for _, r := range lsRows(t, "agents") {
		name := strings.TrimSuffix(r[0], " (off)")
		if slices.Contains(r[1:], "ultra") { // the page shows the resolved profile, not the record's model
			name += "\tultra"
		}
		wants = append(wants, name)
	}
	for _, r := range lsRowsAfter(t, "agents", 1) {
		if len(r) != 3 || r[1] != "launch.env" {
			t.Fatalf("overlay row %q, want agent, launch.env, entries", r)
		}
		for e := range strings.SplitSeq(r[2], ", ") {
			k, v, _ := strings.Cut(e, "=")
			wants = append(wants, r[0]+".launch.env."+k+"\t"+map[bool]string{true: "set", false: "empty"}[v != ""])
		}
	}
	pageCheck(t, out.String(), wants)
	if strings.Contains(out.String(), "hidden-value") {
		t.Errorf("agents page prints an env value:\n%s", out)
	}
}

// The interactive routing page offers every field and the fallback row, shows a staged fallback, fits 80 columns.
func TestRoutingMenuShowsEveryRow(t *testing.T) {
	pageTestEnv(t)
	orig := pickMenu
	t.Cleanup(func() { pickMenu = orig })
	var rows [][2]string
	pickMenu = func(_ string, secs []tui.Section, _ [2]string, _ int) (int, error) {
		for _, s := range secs {
			rows = append(rows, s.Rows...)
		}
		return -1, nil
	}
	runRoutingMenu(&dialog{out: discardWriter{}}, providers.DefaultRouting(), providers.Routing{})
	got := map[string]bool{}
	for _, r := range rows {
		got[r[0]] = true
	}
	want := []string{fallbackRowKey}
	for _, m := range configure.Modules() {
		for _, f := range m.Fields() {
			want = append(want, f.Key)
		}
	}
	for _, k := range want {
		if !got[k] {
			t.Errorf("routing page lacks the %q row: %q", k, rows)
		}
	}
	if !got["weights.headroom"] {
		t.Error("no weights.headroom row")
	}
	secs, _ := routingSections(providers.DefaultRouting(), providers.Routing{}, map[string]string{fallbackRowKey: "all"}, nil)
	if v := secs[0].Rows[len(secs[0].Rows)-1]; v != [2]string{fallbackRowKey, "all (staged)"} {
		t.Errorf("staged fallback row = %q", v)
	}
	assertFitsEighty(t, "routing page", rows)
}

// Weight edits validate against the current weights, with a float-safe bound.
func TestRoutingWeightEditsUseCurrentWeights(t *testing.T) {
	cur := providers.Routing{Weights: map[string]float64{"latency": 0.8, "price": 0.1, "headroom": 0.1}}
	field := func(key string) configure.RoutingField {
		for _, f := range configure.Modules()[0].Fields() {
			if f.Key == key {
				return f
			}
		}
		t.Fatalf("no field %s", key)
		return configure.RoutingField{}
	}
	if err := configure.ValidateRoutingInputOn(field("weights.price"), "0.2", cur); err != nil {
		t.Errorf("price 0.2 with latency 0.8 rejected: %v", err)
	}
	if err := configure.ValidateRoutingInputOn(field("weights.headroom"), "0.3", cur); err == nil {
		t.Error("headroom 0.3 with latency 0.8 accepted")
	}
}

// Agents page rows stay within 80 columns for a 60-character env name and a long profile, and the overlay wins.
func TestAgentsPageRowsFitAndOverlayWins(t *testing.T) {
	reg := pageTestEnv(t)
	long := strings.Repeat("N", 56) + "TAIL"
	vaulttest.SetRecord(t, "codex", "profile", "ultra")
	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"codex":{"launch":{"env":["`+long+`=v"]}}}}`)
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Launch: &vault.AgentLaunch{Env: []string{"OLD_NAME=v"}}}); err != nil {
		t.Fatal(err)
	}
	d, out := newDialog("")
	d.o.Interactive, d.o.DryRun, d.o.Home, d.reg = false, true, os.Getenv("HOME"), reg
	runAgents(d)
	for l := range strings.SplitSeq(out.String(), "\n") {
		if (strings.Contains(l, "launch.env") || strings.Contains(l, "routes to")) && textutil.DisplayWidth(l) > 80 {
			t.Errorf("row of %d cells: %q", textutil.DisplayWidth(l), l)
		}
	}
	if !strings.Contains(out.String(), "TAIL  set") || !strings.Contains(out.String(), "codex.launch.env.N") || strings.Contains(out.String(), "OLD_NAME") {
		t.Errorf("overlay launch must win over the record:\n%s", out)
	}
}
