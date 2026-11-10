package menu

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// homeRow is one selectable home-page row: name, state, check, and what picking it does.
type homeRow = Row[*dialog]

func toolsRows(d *dialog) []homeRow {
	return []homeRow{databricksRow(d), tailscaleRow(), webSearchRow()}
}

func settingsRows(d *dialog) []homeRow {
	n := len(providers.CustomEnv())
	return []homeRow{
		{Name: "secrets", State: "vault", Check: "API keys & tokens", Run: func(d *dialog) { secretsLoop(d) }},
		routingRow(d),
		{Name: "environment", State: fmt.Sprintf("%d vars", n), Check: "injected into every launch", Run: runCustomEnv},
		seedRow(),
	}
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
	if configure.HasAdminCredential() {
		check = "admin key"
	}
	if host != "" {
		check = host + " · " + check
	}
	return homeRow{Name: "tailscale", State: state, Check: check, Run: func(d *dialog) { runRemote(d) }}
}

// webSearchKey is the Tavily key the web_search MCP server reads.
const webSearchKey = "TAVILY_API_KEY"

func webSearchRow() homeRow {
	state := "no key"
	src, err := vault.HasSecretErr(webSearchKey) // presence only: no decrypt per render
	switch {
	case err != nil:
		state = "status unknown"
	case src != "":
		state = "key set"
	}
	var wired []string
	for _, c := range webSearchClients() {
		if c.wired() {
			wired = append(wired, c.name)
		}
	}
	check := "mcp: " + cmp.Or(strings.Join(wired, ","), "none")
	return homeRow{Name: "web search", State: state, Check: check, Run: runWebSearch}
}

// mcpClient is one agent config that can register the web_search MCP server.
type mcpClient struct {
	name     string
	wired    func() bool
	register func(bin string) error
}

func webSearchClients() []mcpClient {
	codexCfg, cfgErr := paths.CodexConfigPath("")
	return []mcpClient{
		{"claude", func() bool { return configure.ClaudeJSONWired(paths.ClaudeJSON()) }, configure.RegisterClaudeMCP},
		{"codex", func() bool { return configure.CodexWired(codexCfg) }, func(bin string) error {
			if cfgErr != nil {
				return cfgErr
			}
			return configure.RegisterCodexMCP(codexCfg, bin)
		}},
	}
}

// runWebSearch sets the Tavily key when missing, then points every client's web_search at claude-code.
func runWebSearch(d *dialog) {
	if _, _, ok := vault.Get(webSearchKey); !ok {
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
		env := stagedCustomEnv(edits)
		names := slices.Sorted(maps.Keys(env))
		rows := customEnvRows(env, names)
		idx, err := d.shell().PickPlain("environment — injected into every launch", rows)
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

// stagedCustomEnv is the vault env with the page's staged edits applied.
func stagedCustomEnv(edits map[string]envEdit) map[string]string {
	env := providers.CustomEnv()
	for k, e := range edits {
		if e.raw == "" {
			delete(env, k)
		} else {
			env[k] = e.raw
		}
	}
	return env
}

// customEnvRows is one row per name (plain value redacted, secret hidden) plus the "+ New" and save rows.
func customEnvRows(env map[string]string, names []string) [][2]string {
	rows := make([][2]string, 0, len(names)+2)
	for _, n := range names {
		kind, shown := "plain", vault.Redact(env[n])
		if env[n] == providers.SecretRef {
			kind, shown = "secret", ""
		}
		rows = append(rows, [2]string{n, strings.TrimSpace(kind + "  " + shown)})
	}
	return append(rows, [2]string{"+ New", "NAME, then value, then plain or secret"}, EnterRow("save and go back"))
}

// editCustomEnv reads one change (no confirmation): NAME for a new var, the value (empty deletes), then
// where it lives. Deleting or switching an @secret var away from secret offers to delete the sealed
// secret it leaves behind, so it doesn't orphan in the vault.
func editCustomEnv(d *dialog, name string) (string, envEdit, bool) {
	isNew := name == ""
	if isNew {
		var ok bool
		if name, ok = readNewEnvName(d); !ok {
			return "", envEdit{}, false
		}
	}
	wasSecret := !isNew && providers.CustomEnv()[name] == providers.SecretRef
	val, err := secretLine(d, "Value for "+name+" (input hidden; empty deletes it): ")
	if err != nil {
		return "", envEdit{}, false
	}
	if val = strings.TrimSpace(val); val == "" {
		del := wasSecret && d.shell().Confirm("also delete the sealed secret "+name+"?")
		return name, envEdit{deleteSecret: del}, true
	}
	return pickEnvStorage(d, name, val, wasSecret)
}

// pickEnvStorage asks whether the non-empty value val lives plain or sealed as a secret, offering to drop the old
// sealed secret when the variable moves off @secret.
func pickEnvStorage(d *dialog, name, val string, wasSecret bool) (string, envEdit, bool) {
	where, err := d.shell().Pick("store "+name+" as", [][2]string{
		{"plain", "value in the vault env"}, {"secret", "value sealed in vault; vault env name = @secret"}})
	if err != nil || where < 0 {
		return "", envEdit{}, false
	}
	if where == 1 {
		return name, envEdit{raw: providers.SecretRef, secret: val}, true
	}
	del := wasSecret && d.shell().Confirm("switching "+name+" to plain; also delete the old sealed secret?")
	return name, envEdit{raw: val, deleteSecret: del}, true
}

// readNewEnvName asks for a new variable's NAME; ok is false when it is empty or not a valid identifier.
func readNewEnvName(d *dialog) (name string, ok bool) {
	name = strings.TrimSpace(d.shell().Line("NAME: "))
	if name == "" {
		return "", false
	}
	if !vault.EnvName.MatchString(name) {
		d.printf("environment: %q is not a valid name (letters, digits, underscore; not starting with a digit)\n", name)
		return "", false
	}
	return name, true
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
	v, _, ok := vault.Get(name)
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
		if err := configure.SaveEnvEdit(name, e.raw, e.secret); err != nil {
			d.printf("environment: %s: %v\n", name, err)
			continue
		}
		d.printf("environment: %s %s\n", name, map[bool]string{true: "removed", false: "saved"}[e.raw == ""])
		if e.deleteSecret {
			if err := configure.DeleteSecret(name); err != nil {
				d.printf("environment: %s: sealed secret not deleted: %v\n", name, err)
				continue
			}
			d.printf("environment: %s: sealed secret deleted\n", name)
		}
	}
	d.changed = true
}

// rerenderConfigs re-renders the agent configs on d; settings.json is the owner's and is never written here.
func rerenderConfigs(d *dialog) error {
	if err := configure.RenderAgentConfigs(d.out); err != nil {
		return fmt.Errorf("agent configs: %w", err)
	}
	return nil
}

// runRemote reports tailscale connectivity and dispatch hosts — read-only, never fails --check.
func runRemote(d *dialog) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	st, err := tailscale.Status(ctx)
	switch err {
	case nil:
		d.printf("tailscale   running (%s)\n", strings.TrimSuffix(st.DNSName, "."))
	default:
		d.printf("tailscale   not connected → claude-code network tailscale --connect\n")
	}
	switch {
	case configure.HasAdminCredential():
		d.printf("admin api   credential present\n")
	default:
		d.printf("admin api   credential absent → claude-code vault set TAILSCALE_OAUTH_CLIENT_ID|_SECRET\n")
	}
	machines, err := tailscale.Machines(ctx)
	switch online := onlineNames(machines); {
	case err != nil:
		d.printf("dispatch    could not list tailnet machines: %v\n", err)
	case len(online) > 0:
		d.printf("dispatch    %s\n", strings.Join(online, ", "))
	default:
		d.printf("dispatch    no online tailnet machines found\n")
	}
	return true
}

// onlineNames is the names of the online machines, in list order.
func onlineNames(machines []tailscale.Machine) []string {
	var online []string
	for _, m := range machines {
		if m.Online && m.Name != "" {
			online = append(online, m.Name)
		}
	}
	return online
}
