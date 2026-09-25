package vendors

// Translates Claude Code's settings (permissions, MCP servers) into opencode's config; best-effort, never errors.

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// claudeImport is the Claude Code settings surface carried into an opencode launch.
type claudeImport struct {
	mcp              map[string]any
	allow, deny, ask []string
	skippedMCP       []string // project .mcp.json servers dropped for lack of ~/.claude.json approval
}

// readClaudeImport merges permissions + approved mcpServers; cwd=="" skips project files, malformed files are ignored.
func readClaudeImport(cwd string) claudeImport {
	var imp claudeImport
	rules := func(perms map[string]any, key string) []string {
		var out []string
		arr, _ := perms[key].([]any)
		for _, r := range arr {
			if s, ok := r.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	settingsPaths := []string{filepath.Join(paths.ConfigDir(), "settings.json")}
	if cwd != "" {
		settingsPaths = append(settingsPaths,
			filepath.Join(cwd, ".claude", "settings.json"),
			filepath.Join(cwd, ".claude", "settings.local.json"),
		)
	}
	for _, p := range settingsPaths {
		perms := jsonField(p, "permissions")
		if perms == nil {
			continue
		}
		imp.allow = append(imp.allow, rules(perms, "allow")...)
		imp.deny = append(imp.deny, rules(perms, "deny")...)
		imp.ask = append(imp.ask, rules(perms, "ask")...)
	}
	mcp := map[string]any{}
	claudeDoc := jsonDoc(paths.ClaudeJSON())
	if global, ok := claudeDoc["mcpServers"].(map[string]any); ok {
		maps.Copy(mcp, global) // global entries are user-approved by definition
	}
	if cwd != "" {
		if project := jsonField(filepath.Join(cwd, ".mcp.json"), "mcpServers"); project != nil {
			enabled, disabled, allEnabled := projectMCPApproval(claudeDoc, cwd)
			for name, raw := range project {
				if disabled[name] || !(allEnabled || enabled[name]) {
					imp.skippedMCP = append(imp.skippedMCP, name)
					continue
				}
				mcp[name] = raw
			}
		}
	}
	imp.mcp = mcp
	return imp
}

// projectMCPApproval reads ~/.claude.json's per-project MCP approval state for cwd (tried as-is, then cleaned).
func projectMCPApproval(doc map[string]any, cwd string) (enabled, disabled map[string]bool, allEnabled bool) {
	allEnabled, _ = doc["enableAllProjectMcpServers"].(bool)
	projects, _ := doc["projects"].(map[string]any)
	proj, ok := projects[cwd].(map[string]any)
	if !ok {
		proj, ok = projects[filepath.Clean(cwd)].(map[string]any)
	}
	if !ok {
		return map[string]bool{}, map[string]bool{}, allEnabled
	}
	toSet := func(field string) map[string]bool {
		out := map[string]bool{}
		arr, _ := proj[field].([]any)
		for _, v := range arr {
			if s, ok := v.(string); ok {
				out[s] = true
			}
		}
		return out
	}
	return toSet("enabledMcpjsonServers"), toSet("disabledMcpjsonServers"), allEnabled
}

// jsonDoc reads path and decodes it into a map, or nil on any missing/malformed/absent case.
func jsonDoc(path string) map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	return doc
}

// jsonField reads path and returns field as a map, or nil on any missing/malformed/absent/non-object case.
func jsonField(path, field string) map[string]any {
	doc := jsonDoc(path)
	if doc == nil {
		return nil
	}
	v, ok := doc[field].(map[string]any)
	if !ok {
		return nil
	}
	return v
}

// applyRule classifies one permission rule into bash/perm maps; returns true when it set the global bash allow.
func applyRule(rule, verdict string, bash, perm map[string]any) bool {
	tool, inner := rule, ""
	if i := strings.IndexByte(rule, '('); i > 0 && strings.HasSuffix(rule, ")") {
		tool, inner = rule[:i], rule[i+1:len(rule)-1]
	}
	switch tool {
	case "Bash":
		if inner == "" || inner == "*" {
			if verdict == "allow" {
				return true
			}
			if _, taken := bash["*"]; !taken {
				bash["*"] = verdict
			}
			return false
		}
		key := inner
		if cmd, ok := strings.CutSuffix(inner, ":*"); ok {
			key = cmd + " *"
		}
		if _, taken := bash[key]; !taken {
			bash[key] = verdict
		}
	case "Edit", "Write":
		if (inner == "" || inner == "*") && perm["edit"] == nil {
			perm["edit"] = verdict
		}
	case "WebFetch":
		if (inner == "" || inner == "*") && perm["webfetch"] == nil {
			perm["webfetch"] = verdict
		}
	}
	return false
}

// permissionBlock maps Claude rules to opencode's shape; deny beats ask beats allow on a collision.
func permissionBlock(allow, deny, ask []string) map[string]any {
	bash := map[string]any{}
	perm := map[string]any{}
	bashGlobalAllow := false
	apply := func(rules []string, verdict string) {
		for _, rule := range rules {
			if applyRule(rule, verdict, bash, perm) {
				bashGlobalAllow = true
			}
		}
	}
	apply(deny, "deny")
	apply(ask, "ask")
	apply(allow, "allow")
	if bashGlobalAllow && len(bash) == 0 {
		bash["*"] = "allow" // no asks/denies to swallow — the blanket allow is safe to carry
	}
	if len(bash) > 0 {
		perm["bash"] = bash
	}
	if len(perm) == 0 {
		return nil
	}
	return perm
}

// mcpBlock converts a stdio entry to {type:local,...} or a url entry to {type:remote,...}; opencode disallows extras.
func mcpBlock(servers map[string]any) map[string]any {
	out := map[string]any{}
	for name, raw := range servers {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if cmd, ok := entry["command"].(string); ok && cmd != "" {
			argv := []any{cmd}
			if args, ok := entry["args"].([]any); ok {
				argv = append(argv, args...)
			}
			oc := map[string]any{"type": "local", "command": argv, "enabled": true}
			if env, ok := entry["env"].(map[string]any); ok && len(env) > 0 {
				oc["environment"] = env
			}
			out[name] = oc
			continue
		}
		if url, ok := entry["url"].(string); ok && url != "" {
			oc := map[string]any{"type": "remote", "url": url, "enabled": true}
			if headers, ok := entry["headers"].(map[string]any); ok && len(headers) > 0 {
				oc["headers"] = headers
			}
			out[name] = oc
		}
	}
	return out
}
