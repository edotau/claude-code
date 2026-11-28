package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/settings"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
	"github.com/edotau/claude-code/internal/routing/agentid"
	"github.com/edotau/claude-code/internal/routing/googletools"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// agyMsgAllowRules are the harness MCP tool grants added to agy's allow list so `agy -p` calls them without prompting.
var agyMsgAllowRules = []string{"mcp__harness__msg_send", "mcp__harness__msg_read", "mcp__harness__gate", "mcp__harness__diff"}

func renderGeminiSettings(home string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	rec, err := agentconfig.Settings(agentid.Gemini)
	if err != nil {
		fmt.Fprintf(out, "gemini-settings: skipped: %v\n", err)
		return false, fmt.Errorf("gemini-settings: agent settings: %w", err)
	}
	if rec.Extras[extraUseGuardRules] == "false" {
		if !legacyGeminiMsg(home) {
			return false, nil
		}
		return agentconfig.CommitJSONObject(agySettingsPath(home), o, out, "gemini-settings", func(obj map[string]any, _ bool) bool {
			return retireGeminiMsgRules(obj)
		})
	}
	cfg, err := loadAgentConfig()
	if err != nil {
		fmt.Fprintf(out, "gemini-settings: skipped: %v\n", err)
		return false, fmt.Errorf("gemini-settings: load agentconfig: %w", err)
	}
	g := cfg.Guard()
	allow := slices.Clone(g.Allow)
	if geminiHarnessAllowed(home) {
		allow = append(allow, agyMsgAllowRules...)
	}
	lists := map[string][]string{
		"allow": agyGuardRules(allow, home, out),
		"ask":   agyGuardRules(g.Ask, home, out),
		"deny":  agyGuardRules(g.Deny, home, out),
	}
	path := agySettingsPath(home)
	return agentconfig.CommitJSONObject(path, o, out, "gemini-settings", func(obj map[string]any, existed bool) bool {
		if !applyAgySettings(obj, existed, path, lists, g.DefaultMode, out) {
			return false
		}
		if legacyGeminiMsg(home) {
			retireGeminiMsgRules(obj)
		}
		return true
	})
}

// renderGeminiMCP merges the harness msg MCP server into ~/.gemini/config/mcp_config.json, keeping other servers.
func renderGeminiMCP(home string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	path := agyMCPPath(home)
	return agentconfig.CommitJSONObject(path, o, out, "gemini-mcp", func(obj map[string]any, _ bool) bool {
		servers := map[string]any{}
		if v, has := obj["mcpServers"]; has {
			var ok bool
			if servers, ok = v.(map[string]any); !ok {
				fmt.Fprintf(out, "gemini-mcp: skipped: %s has a non-object mcpServers\n", path)
				return false
			}
		}
		entry := geminiMCPEntry(false)
		if cur, set := servers["harness"]; set && !sameGeminiMCP(cur, entry) {
			fmt.Fprintf(out, "gemini-mcp: skipped: %s declares its own harness server\n", path)
			return false
		}
		if old, ok := servers["msg"]; ok && sameGeminiMCP(old, geminiMCPEntry(true)) {
			delete(servers, "msg") // the server's name before it carried more than messages
		}
		servers["harness"] = entry
		obj["mcpServers"] = servers
		return true
	})
}

func retireGeminiMsgRules(obj map[string]any) bool {
	perms, ok := agyPermissions(obj)
	if !ok {
		return false
	}
	allow, ok := perms["allow"].([]any)
	if !ok {
		return false
	}
	n := len(allow)
	perms["allow"] = slices.DeleteFunc(allow, func(v any) bool {
		s, _ := v.(string)
		return s == "mcp(msg/msg_send)" || s == "mcp(msg/msg_read)" || s == "mcp__msg__msg_send" || s == "mcp__msg__msg_read"
	})
	return len(perms["allow"].([]any)) != n
}

// A preserved user harness server must not gain the harness's automatic grants.
func geminiHarnessAllowed(home string) bool {
	raw, existed, err := agentconfig.ReadExisting(agyMCPPath(home))
	if err != nil {
		return false
	}
	if !existed || len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil || doc == nil {
		return false
	}
	servers, ok := doc["mcpServers"].(map[string]any)
	if _, set := doc["mcpServers"]; set && !ok {
		return false
	}
	h, set := servers["harness"]
	return !set || sameGeminiMCP(h, geminiMCPEntry(false))
}

func geminiMCPEntry(legacy bool) map[string]any {
	args := []string{"agents", "mcp", "--from", "gemini"}
	if legacy {
		args = []string{"agents", "msg", "--mcp", "--from", "gemini"}
	}
	return map[string]any{"command": filepath.Join(paths.BinDir(), "claude-code"), "args": args}
}

func sameGeminiMCP(a, b any) bool {
	x, ex := json.Marshal(a)
	y, ey := json.Marshal(b)
	return ex == nil && ey == nil && agentconfig.SameJSONObject(x, y)
}

// legacyGeminiMsg is true only for the msg entry we rendered; a user-owned msg grant stays.
func legacyGeminiMsg(home string) bool {
	raw, err := os.ReadFile(agyMCPPath(home))
	var doc struct {
		Servers map[string]any `json:"mcpServers"`
	}
	if err != nil || json.Unmarshal(raw, &doc) != nil {
		return false
	}
	if h, set := doc.Servers["harness"]; set && !sameGeminiMCP(h, geminiMCPEntry(false)) {
		return false
	}
	old, exists := doc.Servers["msg"]
	return sameGeminiMCP(old, geminiMCPEntry(true)) || !exists && sameGeminiMCP(doc.Servers["harness"], geminiMCPEntry(false))
}

// applyAgySettings merges translated allow/ask/deny lists and defaultMode into obj.
func applyAgySettings(obj map[string]any, existed bool, path string, lists map[string][]string, mode string, out io.Writer) bool {
	if len(lists["allow"])+len(lists["ask"])+len(lists["deny"]) == 0 && mode == "" && !existed {
		return false // nothing to sync: don't create agy's settings.json just to write an empty object
	}
	perms, ok := agyPermissions(obj)
	if !ok {
		fmt.Fprintf(out, "gemini-settings: skipped: %s has a non-object permissions or non-array rule list\n", path)
		return false
	}
	for _, k := range []string{"allow", "ask", "deny"} {
		mergeAgyList(perms, k, lists[k])
	}
	obj["permissions"] = perms
	if mode != "" && obj["agentMode"] == nil {
		obj["agentMode"] = mode
	}
	return true
}

// agyPermissions returns obj's permissions map; ok is false when permissions or any rule list has the wrong JSON type.
func agyPermissions(obj map[string]any) (perms map[string]any, ok bool) {
	perms = map[string]any{}
	if v, has := obj["permissions"]; has {
		if perms, ok = v.(map[string]any); !ok {
			return nil, false
		}
	}
	for _, k := range []string{"allow", "ask", "deny"} {
		if v, has := perms[k]; has {
			if _, ok := v.([]any); !ok {
				return nil, false
			}
		}
	}
	return perms, true
}

// mergeAgyList merges rules into perms[key], preserving existing string and non-string entries without duplicates.
func mergeAgyList(perms map[string]any, key string, rules []string) {
	existing, has := perms[key].([]any)
	if len(rules) == 0 && !has {
		return
	}
	merged := []any{}
	for _, d := range mergeAgyDeny(existing, rules) {
		merged = append(merged, d)
	}
	for _, e := range existing { // keep entries agy may define that are not plain strings
		if _, ok := e.(string); !ok {
			merged = append(merged, e)
		}
	}
	perms[key] = merged
}

// agyGuardRules converts Claude Code permission rules into agy action(target) rules, noting any skipped globs.
func agyGuardRules(in []string, home string, out io.Writer) []string {
	var rules []string
	for _, r := range in {
		if rule, ok := agyRule(r, home, out); ok && !slices.Contains(rules, rule) {
			rules = append(rules, rule)
		}
	}
	return rules
}

// agyBareRules maps bare or wildcard Claude Code tool names to their agy global rules.
var agyBareRules = map[string]string{
	"Bash": "command(*)", "Bash(*)": "command(*)",
	"Read": "read_file(*)", "Read(*)": "read_file(*)",
	"Edit": "write_file(*)", "Write": "write_file(*)", "MultiEdit": "write_file(*)",
	"Edit(*)": "write_file(*)", "Write(*)": "write_file(*)",
	"WebFetch": "read_url(*)", "WebSearch": "read_url(*)", "WebFetch(*)": "read_url(*)",
}

// agyRule converts one Claude Code permission rule to its agy action(target) equivalent.
func agyRule(r, home string, out io.Writer) (string, bool) {
	if mapped, ok := agyBareRules[r]; ok {
		return mapped, true
	}
	if srv, tool, ok := strings.Cut(strings.TrimPrefix(r, "mcp__"), "__"); ok && strings.HasPrefix(r, "mcp__") && srv != "" && tool != "" {
		return "mcp(" + srv + "/" + tool + ")", true
	}
	open := strings.IndexByte(r, '(')
	if open <= 0 || !strings.HasSuffix(r, ")") {
		fmt.Fprintf(out, "gemini-settings: note: %q is not a recognized agy rule, skipped\n", r)
		return "", false
	}
	return agyScopedRule(r, r[:open], r[open+1:len(r)-1], home, out)
}

// agyScopedRule converts a Tool(arg) rule to its agy equivalent.
func agyScopedRule(raw, tool, arg, home string, out io.Writer) (string, bool) {
	switch tool {
	case "Bash":
		return agyCommandRule(raw, arg, out)
	case "Read":
		return agyPathRule("read_file", raw, arg, home, out)
	case "Edit", "Write", "MultiEdit":
		return agyPathRule("write_file", raw, arg, home, out)
	case "WebFetch":
		if dom, ok := strings.CutPrefix(arg, "domain:"); ok && dom != "" {
			return "read_url(" + dom + ")", true
		}
	}
	fmt.Fprintf(out, "gemini-settings: note: %q is not expressible as an agy rule, skipped\n", raw)
	return "", false
}

// agyCommandRule converts a Bash(...) argument to an agy command(...) prefix or exact rule.
func agyCommandRule(raw, cmd string, out io.Writer) (string, bool) {
	if prefix, ok := strings.CutSuffix(cmd, ":*"); ok {
		cmd = strings.TrimSpace(prefix)
	} else if i := strings.IndexAny(cmd, "*?["); i >= 0 { // agy matches command rules by token prefix, never by glob
		if strings.Trim(cmd[i:], "*?") != "" || strings.TrimSpace(cmd[:i]) == "" {
			fmt.Fprintf(out, "gemini-settings: note: %q is not a trailing glob; an agy prefix rule would widen it, skipped\n", raw)
			return "", false
		}
		cmd = strings.TrimSpace(cmd[:i])
	}
	if cmd == "" || strings.ContainsAny(cmd, "*?[") {
		fmt.Fprintf(out, "gemini-settings: note: %q has a mid-command wildcard; skipped\n", raw)
		return "", false
	}
	return "command(" + cmd + ")", true
}

// agyPathRule converts a Read/Edit/Write path argument to an agy read_file/write_file prefix rule.
func agyPathRule(action, raw, arg, home string, out io.Writer) (string, bool) {
	for _, suf := range []string{"/**", "/*"} {
		if p, ok := strings.CutSuffix(arg, suf); ok {
			arg = p
			break
		}
	}
	if strings.HasPrefix(arg, "~/") && home != "" {
		arg = filepath.Join(home, arg[2:])
	}
	if !filepath.IsAbs(arg) || strings.ContainsAny(arg, "*?[") {
		fmt.Fprintf(out, "gemini-settings: note: %q uses a non-prefix path glob; enforced by safety-scan hook\n", raw)
		return "", false
	}
	return action + "(" + arg + ")", true
}

// mergeAgyDeny returns the string entries of existing, then rules, without duplicates.
func mergeAgyDeny(existing []any, rules []string) []string {
	all := make([]string, 0, len(existing)+len(rules))
	for _, e := range existing {
		if s, ok := e.(string); ok {
			all = append(all, s)
		}
	}
	seen := map[string]bool{}
	return slices.DeleteFunc(append(all, rules...), func(s string) bool {
		dup := seen[s]
		seen[s] = true
		return dup
	})
}

var loadAgentConfig = agentconfig.LoadConfig

// RenderSettings adds --effort after the default launch env and args.
func (e geminiEngine) RenderSettings(ctx context.Context, s agent.Settings, env, argv []string) ([]string, []string, []string, func(), error) {
	env, argv, emulated, cleanup, err := e.BaseEngine.RenderSettings(ctx, s, env, argv)
	if err != nil {
		return env, argv, emulated, cleanup, err
	}
	argv, emulated, err = agyEffortArgv(s, argv, emulated)
	if err == nil && !agent.GeminiGatewayRow(s.Target.Provider) {
		err = agyLoginGoogleTools(s.Target.Provider, s.Target.Model, s.GoogleTools)
	}
	return env, argv, emulated, cleanup, err
}

// agyEffortArgv sets --effort to s.Effort; beside an effort-suffixed --model (agy refuses both) it is emulated.
func agyEffortArgv(s agent.Settings, argv, emulated []string) ([]string, []string, error) {
	if s.Effort == "" {
		return argv, emulated, nil
	}
	if err := agentid.ValidEffort(agentid.Gemini, s.Effort); err != nil {
		return argv, emulated, fmt.Errorf("gemini: %w", err)
	}
	model := s.Target.Model
	if i := slices.Index(argv, "--model"); i >= 0 && i+1 < len(argv) {
		model = argv[i+1]
	}
	if _, _, ok := agentid.SplitNativeEffort(model); ok {
		return argv, append(emulated, "effort "+s.Effort+": model "+model+" carries its own"), nil
	}
	if agyForeign(model) { // agy refuses --effort for a non-Gemini gateway model; agy has no env or settings equivalent
		return argv, append(emulated, "effort "+s.Effort+": agy takes none for "+model), nil
	}
	if i := slices.Index(argv, "--effort"); i >= 0 && i+1 < len(argv) { // an interactive gateway pin's suffix: s.Effort wins
		argv = slices.Clone(argv)
		argv[i+1] = s.Effort
		return argv, emulated, nil
	}
	return append(argv, "--effort", s.Effort), emulated, nil
}

// agyHooksPath is agy's global hooks.json, where renderGeminiHooks installs the hooks.
func agyHooksPath(home string) string { return filepath.Join(home, ".gemini", "config", "hooks.json") }

// agyMCPPath is agy's global mcp_config.json, where renderGeminiMCP installs the msg MCP server.
func agyMCPPath(home string) string {
	return filepath.Join(home, ".gemini", "config", "mcp_config.json")
}

// agySettingsPath is agy's settings.json, where renderGeminiSettings writes the guard denies.
func agySettingsPath(home string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
}

// agyHookName is the one named hook in agy's hooks.json the harness owns; every render replaces it.
const agyHookName = "claude-code"

// agyToolOrder lists agy tools in rendered-matcher order, each beside its hookspec name.
var agyToolOrder = []struct{ agy, claude string }{
	{"run_command", "Bash"}, {"read_file", "Read"}, {"write_file", "Write"}, {"edit_file", "Edit"},
}

// AgyMatcher is spec matcher m in agy's tool names; ok is false when no alternative names an agy tool.
func AgyMatcher(m string) (string, bool) {
	alts := strings.Split(strings.ReplaceAll(m, "MultiEdit", "Edit"), "|")
	var out []string
	for _, t := range agyToolOrder {
		if slices.Contains(alts, t.claude) {
			out = append(out, t.agy)
		}
	}
	return strings.Join(out, "|"), len(out) > 0
}

// agyHookEvents groups the harness's agy hooks by event: tool events as matcher groups, the rest as flat handlers.
func agyHookEvents() map[string][]any {
	events := map[string][]any{}
	for _, s := range settings.SpecsFor("gemini", "native") {
		handler := map[string]any{"type": "command", "command": settings.AgentCommand("gemini", s), "timeout": s.Seconds()}
		if s.Event != settings.EventPreToolUse && s.Event != settings.EventPostToolUse {
			events[s.Event] = append(events[s.Event], handler)
			continue
		}
		matcher, ok := AgyMatcher(s.Matcher)
		if !ok {
			continue // no agy tool to run on
		}
		events[s.Event] = append(events[s.Event], map[string]any{"matcher": matcher, "hooks": []any{handler}})
	}
	return events
}

// renderGeminiHooks merges the harness's agy hooks into ~/.gemini/config/hooks.json, keeping others.
func renderGeminiHooks(home string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	entry, err := textutil.MarshalIndentNoEscape(agyHookEvents())
	if err != nil {
		return false, fmt.Errorf("gemini-hooks: %w", err)
	}
	path := agyHooksPath(home)
	raw, existed, err := agentconfig.ReadExisting(path)
	if err != nil {
		return false, fmt.Errorf("gemini-hooks: %w", err)
	}
	next, err := spliceAgyHook(raw, bytes.ReplaceAll(entry, []byte("\n"), []byte("\n  ")))
	if err != nil {
		return false, fmt.Errorf("gemini-hooks: cannot parse %s, left untouched: %w", path, err)
	}
	return agentconfig.CommitFile(path, raw, next, existed, o, out, "gemini-hooks")
}

// scanAgyHook is raw's [start, end) agyHookName value spans, the last such value and the last member's end (-1: none).
func scanAgyHook(raw []byte) (spans [][2]int, last json.RawMessage, lastEnd int, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil {
		return nil, nil, 0, err
	} else if t != json.Delim('{') {
		return nil, nil, 0, errors.New("not a JSON object")
	}
	lastEnd = -1
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, nil, 0, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, 0, err
		}
		lastEnd = int(dec.InputOffset())
		if key == agyHookName {
			spans, last = append(spans, [2]int{lastEnd - len(v), lastEnd}), v
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, 0, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, 0, errors.New("trailing data after the top-level object")
	}
	return spans, last, lastEnd, nil
}

// spliceAgyHook returns raw with its top-level agyHookName value set to entry, every other byte kept; raw if equal.
func spliceAgyHook(raw, entry []byte) ([]byte, error) {
	member := []byte(strconv.Quote(agyHookName) + ": ")
	if len(bytes.TrimSpace(raw)) == 0 {
		return slices.Concat([]byte("{\n  "), member, entry, []byte("\n}\n")), nil
	}
	spans, last, lastEnd, err := scanAgyHook(raw)
	if err != nil {
		return nil, err
	}
	switch {
	case len(spans) == 1 && agentconfig.SameJSONObject(last, entry):
		return raw, nil
	case len(spans) > 0:
		for _, span := range slices.Backward(spans) {
			raw = slices.Concat(raw[:span[0]], entry, raw[span[1]:])
		}
		return raw, nil
	case lastEnd < 0: // an empty object
		open, closing := bytes.IndexByte(raw, '{'), bytes.LastIndexByte(raw, '}')+1
		return slices.Concat(raw[:open], []byte("{\n  "), member, entry, []byte("\n}"), raw[closing:]), nil
	}
	return slices.Concat(raw[:lastEnd], []byte(",\n  "), member, entry, raw[lastEnd:]), nil
}

// agyGatewayHeader is the gateway headers value: the google-tools header for a headless run that asked, else "".
func agyGatewayHeader(p *providers.Provider, base string, h *agent.Headless) (string, error) {
	if h == nil || len(h.GoogleTools) == 0 {
		return "", nil
	}
	return agyGoogleToolsHeader(p, base, h.GoogleTools)
}

// agyGoogleToolsHeader is the proxy header for tools; preflight refuses a foreign model or an unserved tool.
func agyGoogleToolsHeader(p *providers.Provider, base string, names []string) (string, error) {
	asked, err := googletools.Parse(names)
	if err != nil {
		return "", err
	}
	if agyForeign(base) {
		return "", fmt.Errorf("gemini: --google-tools needs a Gemini model; %s is not one", base)
	}
	if served := p.Options.GoogleTools; served != nil {
		ok, _ := googletools.Parse(served)
		if miss := googletools.Unserved(ok, asked); len(miss) > 0 {
			return "", &agent.PreflightError{Finding: agent.Finding{Check: "google-tools", Profile: p.Name, Model: base,
				Detail: fmt.Sprintf("%s does not serve google tool %s; served: %s", p.Name,
					googletools.Join(miss), strings.Join(served, ","))}}
		}
	} else {
		fmt.Fprintf(os.Stderr, "gemini: google tools unprobed on %s; sent as asked\n", p.Name)
	}
	// the model token scopes the tools to the main model; agy's flash-lite title calls reach the same gateway
	return googletools.Header + ": " + googletools.Join(asked) + "," +
		googletools.ModelPrefix + base, nil
}

// agySettingsKey maps a tool to its agy --settings key; Phase 0.1 found none, so every tool is refused.
var agySettingsKey = map[googletools.Tool]string{}

// agyLoginGoogleTools refuses a tool agy's own Google login has no setting for.
func agyLoginGoogleTools(p *providers.Provider, model string, names []string) error {
	asked, err := googletools.Parse(names)
	if err != nil {
		return err
	}
	profile := ""
	if p != nil {
		profile = p.Name
	}
	for _, t := range asked {
		if _, ok := agySettingsKey[t]; !ok {
			return &agent.PreflightError{Finding: agent.Finding{Check: "google-tools", Profile: profile, Model: model,
				Detail: "agy's own login has no setting for google tool " + string(t)}}
		}
	}
	return nil
}
