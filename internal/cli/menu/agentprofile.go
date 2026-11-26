// agentprofile.go — the Profile row every agent page carries: the profile an agent runs on, or off; plus record writes.
package menu

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/dev/configure"
	"github.com/edotau/claude-code/internal/driver/codex"
	"github.com/edotau/claude-code/internal/driver/copilot"
	"github.com/edotau/claude-code/internal/driver/copilotauth"
	"github.com/edotau/claude-code/internal/platform/vault"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/cli/ui/tui"
	"github.com/edotau/claude-code/internal/driver/opencode"
	"github.com/edotau/claude-code/internal/platform/parallel"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// agentPinRows is the profile section: Pin / default, Databricks, Model (a record one without a catalog), Fallback.
func agentPinRows(d *dialog, agent string) []billingRow {
	cur := configure.AgentField(vault.GetAgentRecord(agent), "PROFILE")
	provider := "pinned profile"
	if d.reg != nil {
		if p := d.reg.Providers[cur]; p != nil && p.Provider != "" {
			provider = p.Provider
		}
	}
	state := checked(cur != "", seedLabel(agent, cur))
	if why := agentpkg.AgentUnbillable(agent); cur != "" && why != "" {
		state = "[off, " + why + "]"
	}
	rows := []billingRow{{label: "Pin / default", desc: "bare launch: " + provider + " " + state,
		run: func() bool {
			changed, err := pickAgentProfile(d, agent, cur)
			if err != nil {
				d.printf("%s: %v\n", agent, err)
			}
			return changed
		}}}
	if r := dbxWorkspaceRow(d, agent); r != nil {
		rows = append(rows, *r)
	}
	if r := modelRow(d, agent); r != nil {
		rows = append(rows, *r)
	} else if agent != "claude" && agent != "copilot" {
		rows = append(rows, recordModelRow(d, agent))
	}
	return append(rows, fallbackRow(d, agent))
}

// recordModelRow is the Model row for an agent with no catalog picker: it picks the record's model.
func recordModelRow(d *dialog, agent string) billingRow {
	model := cmp.Or(vault.GetAgentRecord(agent).Model, "default")
	return billingRow{label: "Model", desc: textutil.TruncateCells(model, 40, "…"),
		run: func() bool { return pickRecordModel(d, agent) }}
}

// discoverModels is the discovery seam; tests replace it.
var discoverModels = configure.DiscoverModels

// modelIDs is the ids agent serves on profile in mode, else its tier-map ids; err is the discovery failure.
func modelIDs(d *dialog, agent, profile string, mode configure.ModelMode) (ids []string, err error) {
	if configure.CanDiscover(agent) {
		var res configure.ModelResult
		if res, err = discoverModels(context.Background(), configure.ModelQuery{Agent: agent, Profile: profile, Mode: mode}); err == nil {
			for _, m := range res.Models {
				ids = append(ids, m.ID)
			}
		}
	}
	if len(ids) == 0 && d.reg != nil && d.reg.Providers[profile] != nil {
		ids = slices.Compact(slices.Sorted(maps.Values(d.reg.Providers[profile].Models)))
	}
	return ids, err
}

// modelPickRows is the picker table: default, ids (cur marked), then Refresh when live and the typed-id row.
func modelPickRows(ids []string, cur string, live bool) [][2]string {
	rows := [][2]string{{"default (clear)", mark(cur == "")}}
	for _, id := range ids {
		rows = append(rows, [2]string{textutil.TruncateCells(id, 60, "…"), mark(id == cur)})
	}
	if live {
		rows = append(rows, [2]string{"Refresh (discover live)", ""})
	}
	return append(rows, [2]string{"Other… (type an id)", ""})
}

// pickRecordModel picks MODEL from default, served ids, a live refresh and a typed id.
func pickRecordModel(d *dialog, agent string) bool {
	profile := configure.AgentField(vault.GetAgentRecord(agent), "PROFILE")
	ids, err := modelIDs(d, agent, profile, configure.RecallModels)
	if err != nil {
		d.printf("%s: %v\n", agent, err)
	}
	live := configure.CanDiscover(agent)
	for {
		cur := vault.GetAgentRecord(agent).Model
		start := slices.Index(ids, cur) + 1
		idx, err := d.shell().Pick(displayLabel(agent)+" — model", modelPickRows(ids, cur, live), start)
		switch {
		case err != nil || idx < 0:
			return false
		case idx == 0:
			return setAgentField(d, agent, "MODEL", "")
		case idx <= len(ids):
			return setAgentField(d, agent, "MODEL", ids[idx-1])
		case live && idx == len(ids)+1:
			var rerr error
			if ids, rerr = modelIDs(d, agent, profile, configure.RefreshModels); rerr != nil {
				d.printf("%s: %v\n", agent, rerr)
			}
		default:
			return setAgentField(d, agent, "MODEL", strings.TrimSpace(d.shell().Line("Model for "+agent+" (empty clears): ")))
		}
	}
}

// setAgentField writes one of agent's record fields and prints a failure; true when it changed.
func setAgentField(d *dialog, agent, field, val string) bool {
	changed, err := writeAgentField(d, agent, field, val)
	if err != nil {
		d.printf("%s: %v\n", agent, err)
	}
	return changed
}

// fallbackRow is the Fallback row: the profile agent falls back to when its own is unavailable, none when empty.
func fallbackRow(d *dialog, agent string) billingRow {
	cur := configure.AgentField(vault.GetAgentRecord(agent), "FALLBACK")
	desc := "profile fails: " + checked(cur != "", cmp.Or(cur, "none"))
	if agent == "copilot" && cur == "" {
		if fb := copilotFallback(copilot.RouteChain(context.Background(), d.reg)); fb != "" {
			desc = fb
		}
	}
	return billingRow{label: "Fallback", desc: desc,
		run: func() bool {
			names := agentProfileChoices(d, agent)
			rows := make([][2]string, len(names))
			for i, n := range names {
				rows[i] = [2]string{cmp.Or(n, "none"), mark(n == cur)}
			}
			idx, err := d.shell().Pick(displayLabel(agent)+" — fallback profile", rows, max(slices.Index(names, cur), 0))
			if err != nil || idx < 0 || idx >= len(names) {
				return false
			}
			return setAgentField(d, agent, "FALLBACK", names[idx])
		}}
}

// agentProfileChoices is the picker's profile values: off ("") first, then the own login, then the rest.
func agentProfileChoices(d *dialog, agent string) []string {
	names := configure.SeedProfiles(d.reg, agent)
	if agentid.Routing(agent).SubLadder {
		for _, id := range providers.RouteOpencodeSubIDs {
			if !slices.Contains(names, id) {
				names = append(names, id)
			}
		}
	}
	return append([]string{""}, names...)
}

// pickAgentProfile lists off (automatic for claude) and the profiles agent can run on; a pick writes the record.
func pickAgentProfile(d *dialog, agent, cur string) (bool, error) {
	name, ok := pickProfileName(d, agent, agentProfileChoices(d, agent), cur)
	if !ok {
		return false, nil
	}
	if slices.Contains(providers.DatabricksProfiles(d.reg), name) {
		return pinWorkspace(d, agent, name), nil
	}
	return writeAgentField(d, agent, "PROFILE", name)
}

// pickProfileName opens agent's profile picker over names with cur marked; ok is false when dismissed.
func pickProfileName(d *dialog, agent string, names []string, cur string) (name string, ok bool) {
	rows := make([][2]string, len(names))
	for i, n := range names {
		rows[i] = [2]string{seedLabel(agent, n), mark(n == cur)}
	}
	idx, err := d.shell().Pick(displayLabel(agent)+" — profile", rows, max(slices.Index(names, cur), 0))
	if err != nil || idx < 0 || idx >= len(names) {
		return "", false
	}
	return names[idx], true
}

// agentActiveOn turns an off agent on after a successful sign-in, by seeding its own login's profile.
func agentActiveOn(d *dialog, agent string) {
	if agentpkg.VaultAgentIsClaude(agent) || configure.AgentField(vault.GetAgentRecord(agent), "PROFILE") != "" {
		return
	}
	profile := signInProfile(d, agent)
	if profile == "" {
		return
	}
	if _, err := configure.ApplyProfile([]string{agent}, profile); err != nil {
		d.printf("%s: %v\n", agent, err)
		return
	}
	d.changed = true
}

// signInProfile is the profile a sign-in to agent means: its own login's profile, "" when none is registered.
func signInProfile(d *dialog, agent string) string {
	if agent == "claude" && d.reg != nil {
		return providers.ProfileName(d.reg.ProfileBy(anthropicProvider, providers.AuthCLI))
	}
	if agent == "opencode" {
		return opencode.OwnLogin
	}
	if agent == "copilot" {
		return copilotLoginProfile(d.reg, copilotauth.StoredLogin())
	}
	names := configure.SeedProfiles(d.reg, agent)
	if len(names) == 0 || d.reg == nil {
		return ""
	}
	if p := d.reg.Providers[names[0]]; p != nil && p.Auth.Login == agent {
		return names[0]
	}
	return ""
}

// copilotLoginProfile is the github profile whose login is copilot's own login, "" when none matches.
func copilotLoginProfile(reg *providers.Registry, login string) string {
	if reg == nil || login == "" {
		return ""
	}
	for _, n := range reg.ProfilesOf("github") {
		if strings.EqualFold(reg.Providers[n].Login, login) {
			return n
		}
	}
	return ""
}

// writeAgentField sets one record field (a vault.AgentRecord.WithPin name) and prints it; (false, nil) if unchanged.
func writeAgentField(d *dialog, agent, field, val string) (bool, error) {
	if field == "MODEL" {
		if err := configure.ValidateAgentModel(agent, configure.AgentField(vault.GetAgentRecord(agent), "PROFILE"), val); err != nil {
			return false, err
		}
	}
	if changed, err := configure.SetAgentPin(agent, field, val); !changed || err != nil {
		return false, err
	}
	d.printf("  %s %s: %s\n", agent, strings.ToLower(field), cmp.Or(val, "unset"))
	d.changed = true
	return true, nil
}

// Seed section — the "Seed agent profiles" dialog.

const (
	seedOff  = "off"
	seedAuto = "automatic"
)

// seedAgents is claude then every external agent, the dialog's rows in order.
func seedAgents() []string { return append([]string{"claude"}, agentpkg.Names()...) }

// seedLabel is the shown name of a stored profile: "" reads as automatic for claude and off for the rest.
func seedLabel(agent, profile string) string {
	switch {
	case profile != "":
		return profile
	case agentpkg.VaultAgentIsClaude(agent):
		return seedAuto
	}
	return seedOff
}

// seedDefaults is each agent's choice; only a first run (no profile set) pre-selects each agent's own login.
func seedDefaults(d *dialog) map[string]string {
	recs := vault.AgentRecords(agentpkg.RosterNames())
	firstRun := profilesSet(recs) == 0
	out := map[string]string{}
	for _, a := range seedAgents() {
		out[a] = configure.AgentField(recs[a], "PROFILE")
		if firstRun && out[a] == "" && !agentpkg.VaultAgentIsClaude(a) && d.reg != nil {
			out[a] = signInProfile(d, a)
		}
	}
	return out
}

// runSeed shows one row per agent and writes every choice.
func runSeed(d *dialog) {
	choice, agents := seedDefaults(d), seedAgents()
	d.shell().Run("Seed agent profiles", func() []shellRow {
		rows := make([]shellRow, 0, len(agents)+1)
		for _, a := range agents {
			rows = append(rows, shellRow{Name: displayLabel(a), Detail: seedLabel(a, choice[a]), Run: func() bool {
				if p, ok := pickProfileName(d, a, agentProfileChoices(d, a), choice[a]); ok {
					choice[a] = p
				}
				return false
			}})
		}
		return append(rows, shellRow{Name: "Save", Detail: "write these choices", Run: func() bool {
			saveSeed(d, choice)
			return true
		}})
	})
}

// saveSeed stores choice's profiles in one update, leaving every other record field as it was.
func saveSeed(d *dialog, choice map[string]string) bool {
	for a, p := range choice {
		if err := configure.ValidProfile([]string{a}, p); err != nil {
			d.printf("seed: %v\n", err)
			return false
		}
	}
	err := vault.UpdateAgentRecords(func(m map[string]vault.AgentRecord) {
		for a, p := range choice {
			r := m[a]
			r = configure.WithProfile(r, configure.RouteIn(d.reg, p), p)
			m[a] = r
		}
	})
	if err != nil {
		d.printf("seed: %v\n", err)
		return false
	}
	d.changed = true
	d.println("agent profiles saved")
	return true
}

// seedOnFirstRun opens the dialog when no agent has a profile yet; off a terminal it only points at the row.
func seedOnFirstRun(d *dialog, section string) {
	if section != "" || d.o.Check || d.o.DryRun || d.o.JSON || d.o.Agent != "" {
		return
	}
	if profilesSet(vault.AgentRecords(agentpkg.RosterNames())) > 0 {
		return
	}
	if d.o.Interactive && isStdinTTY() {
		runSeed(d)
		return
	}
	d.println("no agent profiles set: run `claude-code configure` → seed agents")
}

// profilesSet counts the records in recs that pin a profile.
func profilesSet(recs map[string]vault.AgentRecord) int {
	n := 0
	for _, r := range recs {
		if configure.AgentField(r, "PROFILE") != "" {
			n++
		}
	}
	return n
}

func seedRow() homeRow {
	n := profilesSet(vault.AgentRecords(agentpkg.RosterNames()))
	return homeRow{Name: "seed agents", State: fmt.Sprintf("%d set", n), Check: "profile each agent runs on", Run: func(d *dialog) { runSeed(d) }}
}

// dbxCheckTimeout bounds the sign-in check that follows a workspace pick.
const dbxCheckTimeout = 10 * time.Second

// dbxWorkspaceRow pins agent to one signed-in Databricks workspace; nil when no workspace has a credential.
func dbxWorkspaceRow(d *dialog, agent string) *billingRow {
	if d.reg == nil {
		return nil
	}
	all := providers.DatabricksProfiles(d.reg)
	signedIn := slices.DeleteFunc(slices.Clone(all), func(n string) bool { return !providers.CredentialPresent(d.reg.Providers[n]) })
	if len(signedIn) == 0 {
		return nil
	}
	cur := configure.AgentField(vault.GetAgentRecord(agent), "PROFILE")
	desc := "workspace: pick one"
	if slices.Contains(all, cur) {
		desc = "workspace: " + textutil.TruncateCells(cur, 40, "…")
	}
	return &billingRow{label: "Databricks", desc: desc, run: func() bool {
		name, ok := pickProfileName(d, agent, signedIn, cur)
		if !ok {
			return false
		}
		changed := pinWorkspace(d, agent, name)
		dbxTokenCheck(d, name)
		return changed
	}}
}

// pinWorkspace pins agent and every other agent on a Databricks workspace to ws; true if agent's pin changed.
func pinWorkspace(d *dialog, agent, ws string) (changed bool) {
	all := providers.DatabricksProfiles(d.reg)
	var moved []string
	for _, a := range seedAgents() {
		if a != agent && !slices.Contains(all, configure.AgentField(vault.GetAgentRecord(a), "PROFILE")) {
			continue
		}
		ch, err := writeAgentField(d, a, "PROFILE", ws)
		switch {
		case err != nil:
			d.printf("%s: %v\n", a, err)
		case ch && a != agent:
			moved = append(moved, a)
		case a == agent:
			changed = ch
		}
	}
	if len(moved) > 0 {
		d.printf("databricks %s: also moved %s\n", ws, strings.Join(moved, ", "))
	}
	return changed
}

// dbxTokenCheck mints ws's token the way a headless ask does and prints one line; the token is never shown.
func dbxTokenCheck(d *dialog, ws string) {
	ctx := providers.WithNotes(providers.WithIdentity(context.Background(), providers.IdentityHeadless), func(string) {})
	auth, err := parallel.Within(ctx, dbxCheckTimeout, func(ctx context.Context) (string, error) {
		res, _, err := dbxMint(ctx, ws)
		return res.AuthType, err
	})
	if _, isPanic := errors.AsType[*parallel.PanicError](err); isPanic {
		err = errors.New("token check failed unexpectedly")
	} else if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s", dbxCheckTimeout)
	}
	if err == nil {
		d.printf("databricks %s: token ok (%s)\n", ws, auth)
		return
	}
	why := strings.Join(strings.Fields(textutil.RedactSecrets(err.Error())), " ")
	d.printf("databricks %s: %s · fix: %s\n", ws, textutil.TruncateCells(why, 70, "…"), providers.LoginCommand(ws))
}

// The anthropic picker row stands for both anthropic billing modes.
const (
	anthropicRow      = "anthropic"
	anthropicProvider = "anthropic"
	anthropicKeyEnv   = "ANTHROPIC_API_KEY"
)

// anthropicLogin memoizes the two anthropic probes for one picker pass.
type anthropicLogin struct {
	reg                *providers.Registry
	loginDone, keyDone bool
	keyOK              bool
	st                 providers.AuthState
}

func (f *anthropicLogin) login() (loggedIn bool) {
	if !f.loginDone {
		f.st = agentpkg.AuthStatus("claude")
		f.loginDone = true
	}
	return f.st.Present
}

func (f *anthropicLogin) key() bool {
	if !f.keyDone {
		if p := f.reg.ProfileBy(anthropicProvider, providers.AuthKey); p != nil {
			f.keyOK = providers.CredentialPresent(p)
		}
		f.keyDone = true
	}
	return f.keyOK
}

// claudeMenu is the claude agent's submenu: the two anthropic billing modes, then Login and OAuth token.
func claudeMenu(d *dialog) agentMenu {
	fp := &anthropicLogin{reg: d.reg}
	cur := configure.CurrentProvider(d.reg)
	loggedIn := fp.login()
	keyOK := fp.key()
	tier := agentpkg.AuthPlan(context.Background(), "claude", fp.st)
	return agentMenu{claudeState(d, fp, cur, tier), []billingRow{
		{"Subscription", "OAuth · plan limits · Remote Control " + bracket(loggedIn, withTier("logged in", tier.Name), "not logged in"), modeSubscription, func() bool {
			if !loggedIn {
				if !d.shell().Confirm("Not logged in to claude.ai — run `claude auth login` now?") {
					return false
				}
				if err := configure.RunLogin([]string{"claude", "auth", "login"}); err != nil {
					d.printf("anthropic: %v\n", err)
					return false
				}
			}
			return keepOrPin(d, cur, providers.ProfileName(d.reg.ProfileBy(anthropicProvider, providers.AuthCLI)))
		}},
		{"API key", "pay per token · no Remote Control " + bracket(keyOK, "key set", "no key"), modeAPIKey, func() bool {
			kt := configure.KeyTarget{Label: anthropicRow, EnvVar: anthropicKeyEnv, FixCmd: "claude-code vault set secrets " + anthropicKeyEnv,
				Provider: fp.reg.ProfileBy(anthropicProvider, providers.AuthKey)}
			if !ensureKey(d, kt, keyOK) {
				d.printf("anthropic: no usable API key; left on %s\n", billingLabel(d.reg, cur))
				return false
			}
			return keepOrPin(d, cur, providers.ProfileName(d.reg.ProfileBy(anthropicProvider, providers.AuthKey)))
		}},
		{"Login", "browser sign-in (claude auth login)", "", func() bool {
			return regenerate(d, configure.KeyTarget{Label: anthropicRow}, []string{"claude", "auth", "login"}, false)
		}},
		{"OAuth token", "headless token (" + providers.OAuthTokenEnv + ")", "", func() bool {
			kt := configure.KeyTarget{Label: anthropicRow, EnvVar: providers.OAuthTokenEnv}
			return regenerate(d, kt, []string{"claude", "setup-token"}, true)
		}},
	}}
}

// claudeState is the pinned backend's mode and check; a non-anthropic pin shows by name.
func claudeState(d *dialog, fp *anthropicLogin, cur string, tier providers.Subscription) agentState {
	p, err := d.reg.Get(cur)
	if err != nil {
		return agentState{mode: billingLabel(d.reg, cur), verified: false}
	}
	switch {
	case p.Provider == anthropicProvider && p.Auth.Method() == providers.AuthCLI:
		return agentState{modeSubscription, tier, fp.login()}
	case p.Provider == anthropicProvider:
		return agentState{mode: modeAPIKey, verified: fp.key() && configure.KeyVerified(p)}
	case p.Auth.Login != "":
		return agentState{mode: billingLabel(d.reg, cur), verified: agentpkg.AuthStatus(p.Auth.Login).Present}
	}
	return agentState{mode: billingLabel(d.reg, cur), verified: configure.KeyVerified(p)}
}

// keepOrPin pins name unless it is already the session provider.
func keepOrPin(d *dialog, cur, name string) bool {
	if name == "" {
		d.printf("provider   no matching anthropic profile; left on %s\n", cur)
		return false
	}
	if name == cur || !switchConfirmed(d, cur, name) {
		d.printf("provider   pinned %s (kept)\n", cur)
		return true
	}
	return applyProvider(d, name)
}

// switchConfirmed asks before a terminal pick re-points every new Claude Code launch.
func switchConfirmed(d *dialog, cur, name string) bool {
	if !d.o.Interactive || !d.onTerminal() {
		return true
	}
	return tui.Confirm("Run every new Claude Code launch on "+name+" instead of "+cur+"?", false)
}

// billingLabel names cur as the picker shows it.
func billingLabel(reg *providers.Registry, cur string) string {
	if p, err := reg.Get(cur); err == nil && p.Provider == anthropicProvider {
		if p.Auth.Method() == providers.AuthCLI {
			return "subscription"
		}
		return "API key"
	}
	return backendLabel(reg, cur)
}

// backendLabel names a non-Anthropic profile as provider-profile and its key type: "databricks-qa · API key".
func backendLabel(reg *providers.Registry, cur string) string {
	p, err := reg.Get(cur)
	if err != nil {
		return cur
	}
	name := cur
	if p.Provider != "" && p.Provider != cur {
		name = p.Provider + "-" + cur
	}
	if p.Auth.Method() == providers.AuthCLI || p.Auth.Login != "" {
		return name + " · subscription"
	}
	return name + " · API key"
}

func codexPreferenceValue(key string, overlay, live map[string]string) (value, source string) {
	if value, ok := overlay[key]; ok {
		return value, "vault"
	}
	if value := live[key]; value != "" {
		return value, "native"
	}
	return "Codex default", "default"
}

func runCodexSettings(d *dialog) bool {
	ov, live, path, err := loadCodexSettings(d.o.Home)
	if err != nil {
		d.printf("codex: %v\n", err)
		return false
	}
	d.printf("codex settings: %s\n", path)
	if d.interactive() {
		return runCodexSettingsMenu(d, ov, live)
	}
	return reportCodexSettings(d, ov, live)
}

// loadCodexSettings reads the codex record's preferences, the native settings under home, and the native config path.
func loadCodexSettings(home string) (ov, live map[string]string, path string, err error) {
	if ov, err = codex.LoadCodexPreferences(); err != nil {
		return nil, nil, "", err
	}
	if live, err = codex.ReadCodexSettings(home); err != nil {
		return nil, nil, "", err
	}
	if path, err = paths.CodexConfigPath(home); err != nil {
		return nil, nil, "", err
	}
	return ov, live, path, nil
}

// reportCodexSettings prints one line per field (value, source) with no prompt or write; false on a drift in --check.
func reportCodexSettings(d *dialog, ov, live map[string]string) bool {
	drift, err := codex.CodexSettingsDrift(d.o.Home)
	if err != nil {
		d.printf("codex: %v\n", err)
		return false
	}
	for _, field := range codex.CodexFields() {
		value, source := codexPreferenceValue(field.Key, ov, live)
		if saved, exists := ov[field.Key]; exists && !field.Local && live[field.Key] != saved {
			source += " · live differs"
		}
		d.printf("%s  %s  %s\n", field.Key, value, source)
	}
	if drift {
		d.println("codex: saved preferences differ from native config")
	}
	return !d.o.Check || !drift
}

// runCodexSettingsMenu stages choices; only the explicit Save row writes or applies them.
func runCodexSettingsMenu(d *dialog, ov, live map[string]string) bool {
	next := maps.Clone(ov)
	fields := codex.CodexFields()
	for {
		rows := codexSettingRows(fields, ov, next, live)
		idx, err := d.shell().pickPlain("Codex settings", rows)
		if err != nil {
			d.printf("codex: %v\n", err)
			return false
		}
		if idx < 0 {
			return true
		}
		if idx == len(fields) {
			return saveCodexSettings(d, ov, next)
		}
		if err := stageCodexChoice(d, fields[idx], next); err != nil {
			d.printf("codex: %v\n", err)
			return false
		}
	}
}

// codexSettingRows is one "<key> → value · source" row per field plus the trailing Save row.
func codexSettingRows(fields []codex.CodexField, ov, next, live map[string]string) [][2]string {
	rows := make([][2]string, 0, len(fields)+1)
	for _, f := range fields {
		value, source := codexPreferenceValue(f.Key, next, live)
		if _, existed := ov[f.Key]; existed {
			if _, kept := next[f.Key]; !kept {
				value, source = "Codex default", "after save"
			}
		}
		rows = append(rows, [2]string{f.Key, value + " · " + source})
	}
	return append(rows, [2]string{"Save and apply", "Codex only"})
}

// stageCodexChoice asks for f's value and stages it in next ("Codex default" drops it); a cancel stages nothing.
func stageCodexChoice(d *dialog, f codex.CodexField, next map[string]string) error {
	choices := make([][2]string, 0, len(f.Values)+1)
	for _, value := range f.Values {
		choices = append(choices, [2]string{value, ""})
	}
	choices = append(choices, [2]string{"Codex default", "remove harness override"})
	choice, err := d.shell().Pick(f.Key, choices)
	if err != nil || choice < 0 {
		return err
	}
	if choice == len(f.Values) {
		delete(next, f.Key)
	} else {
		next[f.Key] = f.Values[choice]
	}
	return nil
}

func saveCodexSettings(d *dialog, old, next map[string]string) bool {
	saved, err := configure.SaveCodexSettings(d.o.Home, old, next)
	if !saved {
		d.printf("codex: %v\n", err)
		return false
	}
	if err != nil {
		d.printf("codex: preferences saved; native apply failed: %v\n", err)
		return false
	}
	d.println("codex: preferences saved and applied")
	return true
}
