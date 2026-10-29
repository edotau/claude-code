// home.go — the configure home page's tools and settings sections, and the settings re-render on exit.
package configure

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/browser"
	"github.com/edotau/claude-code/internal/codexsettings"
	"github.com/edotau/claude-code/internal/generate"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/tailscale"
	"github.com/edotau/claude-code/internal/tui"
)

// homeRow is one selectable home-page row: name, state, check, and what picking it does.
type homeRow struct {
	name, state, check string
	run                func(*dialog)
}

func toolsRows(d *dialog) []homeRow {
	return []homeRow{tailscaleRow(), webSearchRow(), browsersRow()}
}

func settingsRows(d *dialog) []homeRow {
	n := len(providers.CustomEnv())
	return []homeRow{
		{"secrets", "vault", "API keys & tokens", func(d *dialog) { secretsLoop(d) }},
		claudeSettingsRow(d),
		routingRow(d),
		{"environment", fmt.Sprintf("%d vars", n), "injected into every launch", runCustomEnv},
		databricksRow(d),
	}
}

// rowsOf lays homeRows out like the agent rows (name · state · check).
func rowsOf(rs []homeRow) ([][2]string, []func(*dialog)) {
	names := make([]string, len(rs))
	cols := make([][2]string, len(rs))
	acts := make([]func(*dialog), len(rs))
	for i, r := range rs {
		names[i], cols[i], acts[i] = r.name, [2]string{r.state, r.check}, r.run
	}
	return withChecks(names, cols), acts
}

func tailscaleRow() homeRow {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	state, host := "not connected", ""
	if st, err := tailscale.Status(ctx); err == nil {
		state = "running"
		host, _, _ = strings.Cut(st.DNSName, ".")
	}
	check := "no admin key"
	if hasAdminCredential() {
		check = "admin key"
	}
	if host != "" {
		check = host + " · " + check
	}
	return homeRow{"tailscale", state, check, func(d *dialog) { runRemote(d) }}
}

func browsersRow() homeRow {
	rep := browser.Detect(browser.NewEnv())
	state := "default " + cmp.Or(rep.Default, "none")
	n := 0
	for _, b := range rep.Browsers {
		if b.Installed {
			n++
		}
	}
	return homeRow{"browsers", state, fmt.Sprintf("%d installed", n), func(d *dialog) { runBrowser(d) }}
}

// webSearchArgs is the claude-code argv every client registers for the web_search MCP server.
var webSearchArgs = []string{"network", "websearch"}

// webSearchKey is the Tavily key the web_search MCP server reads.
const webSearchKey = "TAVILY_API_KEY"

func webSearchRow() homeRow {
	state := "no key"
	if _, _, ok := secrets.Get(webSearchKey); ok {
		state = "key set"
	}
	var wired []string
	for _, c := range webSearchClients() {
		if c.wired() {
			wired = append(wired, c.name)
		}
	}
	check := "mcp: " + cmp.Or(strings.Join(wired, ","), "none")
	return homeRow{"web search", state, check, runWebSearch}
}

// mcpClient is one agent config that can register the web_search MCP server.
type mcpClient struct {
	name     string
	wired    func() bool
	register func(bin string) error
}

func webSearchClients() []mcpClient {
	home, _ := os.UserHomeDir()
	codexCfg, cfgErr := codexsettings.ConfigPath("")
	return []mcpClient{
		{"claude", func() bool { return claudeJSONWired(filepath.Join(home, ".claude.json")) }, registerClaudeMCP},
		{"codex", func() bool { return codexWired(codexCfg) }, func(bin string) error {
			if cfgErr != nil {
				return cfgErr
			}
			return registerCodexMCP(codexCfg, bin)
		}},
	}
}

// claudeJSONWired: ~/.claude.json's user-scope web_search runs `claude-code network websearch`.
func claudeJSONWired(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return false
	}
	s, ok := cfg.MCPServers["web_search"]
	return ok && filepath.Base(s.Command) == "claude-code" && slices.Equal(s.Args, webSearchArgs)
}

// codexMCPBlock matches config.toml's [mcp_servers.web_search] table up to the next table.
var codexMCPBlock = regexp.MustCompile(`(?m)^\[mcp_servers\.web_search\]\n(?:[^\[\n].*\n|\n)*`)

func codexWired(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	b := codexMCPBlock.Find(raw)
	return b != nil && strings.Contains(string(b), "claude-code") && strings.Contains(string(b), `args = ["network", "websearch"]`)
}

func registerClaudeMCP(bin string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return registerClaudeMCPContext(ctx, bin)
}

func registerClaudeMCPContext(ctx context.Context, bin string) error {
	if err := ctxErr(ctx, "claude mcp"); err != nil {
		return err
	}
	claude, err := paths.VendorBin("claude")
	if err != nil {
		return err
	}
	_ = exec.CommandContext(ctx, claude, "mcp", "remove", "-s", "user", "web_search").Run()
	if err := ctxErr(ctx, "claude mcp remove"); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, claude, "mcp", "add", "-s", "user", "web_search", "--", bin, "network", "websearch").CombinedOutput()
	if cerr := ctxErr(ctx, "claude mcp add"); cerr != nil {
		return cerr
	}
	if err != nil {
		return fmt.Errorf("claude mcp add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// registerCodexMCP replaces (or appends) the web_search table, sealing the previous file in the vault backups.
func registerCodexMCP(path, bin string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	block := fmt.Sprintf("[mcp_servers.web_search]\ncommand = %q\nargs = [\"network\", \"websearch\"]\n\n", bin)
	next := codexMCPBlock.ReplaceAllLiteral(raw, []byte(block))
	if !codexMCPBlock.Match(raw) {
		next = append(append(raw, '\n'), block...)
	}
	if len(raw) > 0 {
		if err := codexsettings.Backup(path, raw, true); err != nil {
			return err
		}
	}
	return paths.AtomicWrite(path, next, 0o600)
}

// runWebSearch sets the Tavily key when missing, then points every client's web_search at claude-code.
func runWebSearch(d *dialog) {
	if _, _, ok := secrets.Get(webSearchKey); !ok {
		val, err := secretLine(d, "Paste "+webSearchKey+" (app.tavily.com → API keys; input hidden): ")
		if err != nil || strings.TrimSpace(val) == "" {
			d.println("web search: no key entered; nothing changed")
			return
		}
		if !storeValue(d, webSearchKey, strings.TrimSpace(val)) {
			return
		}
	}
	bin := filepath.Join(paths.BinDir(), "claude-code")
	for _, c := range webSearchClients() {
		if c.wired() {
			d.printf("web search: %s already runs claude-code network websearch\n", c.name)
			continue
		}
		if err := c.register(bin); err != nil {
			d.printf("web search: %s: %v\n", c.name, err)
			continue
		}
		d.printf("web search: %s now runs %s network websearch\n", c.name, bin)
	}
	d.changed = true // opencode inherits Claude's MCP list on the re-render below
}

// envEdit stages a vault env change: raw "" deletes; secret holds the value to seal for an @secret ref;
// deleteSecret asks saveEnvEdits to also delete the sealed secret this name used to point at.
type envEdit struct {
	raw, secret  string
	deleteSecret bool
}

// runCustomEnv edits the vault env bucket, staging changes until the page is left.
func runCustomEnv(d *dialog) {
	edits := map[string]envEdit{}
	defer saveEnvEdits(d, edits)
	for {
		env := providers.CustomEnv()
		for k, e := range edits {
			if e.raw == "" {
				delete(env, k)
			} else {
				env[k] = e.raw
			}
		}
		names := slices.Sorted(maps.Keys(env))
		rows := make([][2]string, 0, len(names)+2)
		for _, n := range names {
			kind, shown := "plain", secrets.Redact(env[n])
			if env[n] == providers.SecretRef {
				kind, shown = "secret", ""
			}
			rows = append(rows, [2]string{n, strings.TrimSpace(kind + "  " + shown)})
		}
		rows = append(rows, [2]string{"+ New", "NAME, then value, then plain or secret"}, enterRow("save and go back"))
		idx, err := pickPlain(d, "environment — injected into every launch", rows)
		if err != nil || idx < 0 || idx == len(rows)-1 {
			return
		}
		name := ""
		if idx < len(names) {
			name = names[idx]
		}
		if n, e, ok := editCustomEnv(d, name); ok {
			if sameAsSaved(n, e) {
				delete(edits, n) // back to what is on disk: nothing left to save
			} else {
				edits[n] = e
			}
		}
	}
}

// editCustomEnv reads one change (no confirmation): NAME for a new var, the value (empty deletes), then
// where it lives. Deleting or switching an @secret var away from secret offers to delete the sealed
// secret it leaves behind, so it doesn't orphan in the vault.
func editCustomEnv(d *dialog, name string) (string, envEdit, bool) {
	isNew := name == ""
	if isNew {
		name = strings.TrimSpace(textLine(d, "NAME: "))
		if name == "" {
			return "", envEdit{}, false
		}
		if !envName.MatchString(name) {
			d.printf("environment: %q is not a valid name (letters, digits, underscore; not starting with a digit)\n", name)
			return "", envEdit{}, false
		}
	}
	wasSecret := !isNew && providers.CustomEnv()[name] == providers.SecretRef
	val, err := secretLine(d, "Value for "+name+" (input hidden; empty deletes it): ")
	if err != nil {
		return "", envEdit{}, false
	}
	if val = strings.TrimSpace(val); val == "" {
		del := wasSecret && confirm(d, "also delete the sealed secret "+name+"?")
		return name, envEdit{deleteSecret: del}, true
	}
	where, err := pickPlain(d, "store "+name+" as", [][2]string{
		{"plain", "value in the vault env"}, {"secret", "value sealed in vault; vault env name = @secret"}})
	if err != nil || where < 0 {
		return "", envEdit{}, false
	}
	if where == 1 {
		return name, envEdit{raw: providers.SecretRef, secret: val}, true
	}
	del := wasSecret && confirm(d, "switching "+name+" to plain; also delete the old sealed secret?")
	return name, envEdit{raw: val, deleteSecret: del}, true
}

// sameAsSaved reports an edit matching the vault env and, for a ref, the stored secret.
func sameAsSaved(name string, e envEdit) bool {
	cur := providers.CustomEnv()[name]
	if e.raw != cur {
		return false
	}
	if e.secret == "" {
		return true
	}
	v, _, ok := secrets.Get(name)
	return ok && v == e.secret
}

// saveEnvEdits asks once, on leaving the page, whether to write the page's changes; N drops them all.
func saveEnvEdits(d *dialog, edits map[string]envEdit) {
	if len(edits) == 0 {
		return
	}
	if !confirmLeave(len(edits), "environment change(s)") {
		d.printf("environment: %d change(s) discarded\n", len(edits))
		return
	}
	for _, name := range slices.Sorted(maps.Keys(edits)) {
		e := edits[name]
		if e.secret != "" {
			if err := secrets.Store(name, e.secret); err != nil {
				d.printf("environment: %s: %v\n", name, err)
				continue
			}
		}
		if err := providers.SetCustomEnv(name, e.raw); err != nil {
			d.printf("environment: %s: %v\n", name, err)
			continue
		}
		d.printf("environment: %s %s\n", name, map[bool]string{true: "removed", false: "saved"}[e.raw == ""])
		if e.deleteSecret {
			if err := secrets.Delete(name); err != nil {
				d.printf("environment: %s: sealed secret not deleted: %v\n", name, err)
				continue
			}
			d.printf("environment: %s: sealed secret deleted\n", name)
		}
	}
	d.changed = true
}

// pickPlain is tui.Pick on the terminal (the list's own Done is its exit, so no Back row), else the numbered prompt.
func pickPlain(d *dialog, title string, rows [][2]string) (int, error) {
	if isStdinTTY() && d.o.In == os.Stdin {
		return tui.Pick(title, rows, 0)
	}
	return pick(d, title, rows)
}

// rerenderConfigs re-renders settings.json (merge: foreign keys/hooks kept, backup first) and the vendor configs;
// false when settings.json could not be brought current.
func rerenderConfigs(d *dialog) bool {
	file := launch.SettingsFile()
	existing, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		d.printf("settings.json: %v\n", err)
		return false
	}
	rendered, err := settings.RenderLive()
	if err != nil {
		d.printf("settings.json: %v\n", err)
		return false
	}
	dropped, derr := settings.LoadDropped() // the vault drop list; unavailable → merge as if empty
	if derr != nil {
		d.printf("settings: drop list unavailable: %v\n", derr)
	}
	merged, _, err := settings.Merge(existing, rendered, dropped)
	switch {
	case err != nil:
		d.printf("settings.json: %v\n", err)
		return false
	case string(merged) == string(existing):
		d.println("settings.json unchanged")
	default:
		backup, err := settings.WriteWithBackup(file, merged)
		if err != nil {
			d.printf("settings.json: %v\n", err)
			return false
		}
		d.printf("settings.json updated (backup %s)\n", backup)
	}
	if _, err := generate.Run(generate.Options{Out: d.out}); err != nil {
		d.printf("agent configs: %v\n", err)
	}
	return true
}
