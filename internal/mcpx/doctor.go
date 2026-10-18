// Package mcpx is a read-only diagnostic for Claude Code's MCP config surface: it lists every
// defined server, runs `claude mcp list` for live connect health, flags a server name defined with
// different commands/urls across scopes, and flags inline plaintext secrets. Ported from claude-dbx's
// internal/mcpx/doctor.go (that repo is being retired); the Databricks-specific registry-token-file
// check and secret-shape helper were dropped — not applicable outside that harness.
package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/secrets/shapes"
)

// listTimeout bounds `claude mcp list`; without it a cold-starting npx server hangs Run() forever.
var listTimeout = 30 * time.Second // a var: test seam

// serverDef is the slice of an MCP server block this doctor inspects (stdio and http/sse shapes).
type serverDef struct {
	Command string            `json:"command,omitempty"`
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// MarshalJSON redacts at the type: a serverDef can reach any JSON dump (report, describe, a future debug
// verb) only with credential-shaped env/header values scrubbed and a has_secrets flag in their place.
func (d serverDef) MarshalJSON() ([]byte, error) {
	type plain serverDef // no MarshalJSON, so the alias cannot recurse
	v := plain(d)
	v.Env, v.Headers = shapes.RedactMap(d.Env), shapes.RedactMap(d.Headers)
	if v.URL != "" {
		v.URL = queryParamSecret.ReplaceAllString(v.URL, "${1}REDACTED")
	}
	return json.Marshal(struct {
		plain
		HasSecrets bool `json:"has_secrets,omitempty"`
	}{v, shapes.Any(d.Env) || shapes.Any(d.Headers) || (d.URL != "" && queryParamSecret.MatchString(d.URL))})
}

// display renders the "command args" / url / type / "?" column.
func (d serverDef) display() string {
	head := d.Command
	if head == "" {
		head = d.URL
	}
	if head == "" {
		head = d.Type
	}
	if head == "" {
		head = "?"
	}
	if len(d.Args) > 0 {
		return head + " " + strings.Join(d.Args, " ")
	}
	return head
}

// Def is one (server, scope) definition row.
type Def struct {
	Server string    `json:"server"`
	Scope  string    `json:"scope"`
	D      serverDef `json:"def"`
}

// CollectDefs reads the user config (top-level mcpServers + per-project blocks under "projects"),
// every project .mcp.json, and every settings file (settings.json/settings.local.json) that carries
// a top-level mcpServers block. Unreadable/invalid files are skipped — an absent config is a clean
// state, not an error.
func CollectDefs(userPath string, projectFiles, settingsFiles []string) []Def {
	var out []Def
	var top struct {
		MCPServers map[string]serverDef `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers map[string]serverDef `json:"mcpServers"`
		} `json:"projects"`
	}
	if data, err := os.ReadFile(userPath); err == nil && json.Unmarshal(data, &top) == nil {
		for name, d := range top.MCPServers {
			out = append(out, Def{name, "user", d})
		}
		for proj, p := range top.Projects {
			for name, d := range p.MCPServers {
				out = append(out, Def{name, "local:" + proj, d})
			}
		}
	}
	out = append(out, readFlatScoped(projectFiles, "project:")...)
	out = append(out, readFlatScoped(settingsFiles, "settings:")...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Scope < out[j].Scope
	})
	return out
}

// readFlatScoped reads a flat {"mcpServers": {...}} file (.mcp.json, settings.json), deduped by
// resolved path, and tags each server with prefix+resolvedPath as its scope.
func readFlatScoped(files []string, prefix string) []Def {
	var out []Def
	seen := map[string]bool{}
	for _, f := range files {
		rp := f
		if r, err := filepath.EvalSymlinks(f); err == nil {
			rp = r
		}
		if seen[rp] {
			continue
		}
		seen[rp] = true
		var doc struct {
			MCPServers map[string]serverDef `json:"mcpServers"`
		}
		data, err := os.ReadFile(rp)
		if err != nil || json.Unmarshal(data, &doc) != nil {
			continue
		}
		for name, d := range doc.MCPServers {
			out = append(out, Def{name, prefix + rp, d})
		}
	}
	return out
}

// mcpList runs `claude mcp list`, capturing stdout+stderr through a regular FILE — never a pipe. The
// node CLI truncates a piped stdout at a power-of-two boundary when it exits without flushing
// (measured in claude-dbx: 10/12 pipe reads short, 0/12 file reads), and a dropped failure line reads
// as an all-clear.
func mcpList(claude string) ([]byte, error) {
	f, err := os.CreateTemp("", "claude-mcp-list-*.txt")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, "mcp", "list")
	cmd.Stdout, cmd.Stderr = f, f
	runErr := cmd.Run()
	if err := f.Close(); err != nil && runErr == nil {
		runErr = err
	}
	out, err := os.ReadFile(f.Name())
	if err != nil {
		return nil, err
	}
	return out, runErr
}

// HealthResult is the outcome of a live `claude mcp list` connect check.
type HealthResult struct {
	Lines     []string `json:"lines,omitempty"`
	Connected int      `json:"connected"`
	Failed    int      `json:"failed"`
	Error     string   `json:"error,omitempty"`
	Skipped   string   `json:"skipped,omitempty"` // fail-open reason: live health was not run
}

// RunHealth runs `claude mcp list` and counts connected vs failed servers. claude=="" fails OPEN
// (reported, not treated as a Failed server) since there is nothing to connect with.
func RunHealth(claude string) HealthResult {
	if claude == "" {
		return HealthResult{Skipped: "claude not on PATH"}
	}
	out, listErr := mcpList(claude)
	var res HealthResult
	for _, ln := range strings.Split(string(out), "\n") {
		switch {
		case strings.Contains(ln, "✔"):
			res.Connected++
		case strings.Contains(ln, "✘"):
			res.Failed++
		default:
			continue
		}
		res.Lines = append(res.Lines, strings.TrimSpace(ln))
	}
	if listErr != nil {
		res.Error = listErr.Error()
	}
	return res
}

// Conflict is a server name defined in more than one scope; Real is true only when the resolved
// commands/urls disagree — identical definitions across scopes are benign layering.
type Conflict struct {
	Server string `json:"server"`
	Rows   []Def  `json:"rows"`
	Real   bool   `json:"real"`
}

// FindConflicts groups defs by server name and flags the ones with disagreeing resolved commands.
// home expands ${HOME} / ~/ so equivalent targets compare equal across scopes.
func FindConflicts(defs []Def, home string) []Conflict {
	byName := map[string][]Def{}
	var names []string
	for _, d := range defs {
		if len(byName[d.Server]) == 0 {
			names = append(names, d.Server)
		}
		byName[d.Server] = append(byName[d.Server], d)
	}
	sort.Strings(names)
	var out []Conflict
	for _, name := range names {
		rows := byName[name]
		if len(rows) < 2 {
			continue
		}
		resolved := map[string]bool{}
		for _, r := range rows {
			cmd := strings.ReplaceAll(r.D.display(), "${HOME}", home)
			cmd = strings.ReplaceAll(cmd, "~/", home+"/")
			resolved[cmd] = true
		}
		out = append(out, Conflict{Server: name, Rows: rows, Real: len(resolved) > 1})
	}
	return out
}

// SecretFinding is one inline plaintext credential flagged in a server definition.
type SecretFinding struct {
	Scope  string `json:"scope"`
	Server string `json:"server"`
	Field  string `json:"field"` // env | headers | url
	Key    string `json:"key,omitempty"`
}

// bearerToken matches an Authorization-style "Bearer <40+ chars>" value.
// queryParamSecret matches a plaintext token=/key= query parameter in a URL; group 1 keeps the
// separator and param name so redact() can replace just the value.
var queryParamSecret = regexp.MustCompile(`(?i)([?&](?:token|key)=)[^&\s]+`)

// looksLikeSecret is the shared credential-shape heuristic (internal/secrets/shapes).
func looksLikeSecret(val string) bool { return shapes.Looks(val) }

// FindSecrets scans every server's env, headers, and url for inline plaintext credentials.
func FindSecrets(defs []Def) []SecretFinding {
	var out []SecretFinding
	for _, d := range defs {
		out = append(out, scanMap(d, "env", d.D.Env)...)
		out = append(out, scanMap(d, "headers", d.D.Headers)...)
		if d.D.URL != "" && queryParamSecret.MatchString(d.D.URL) {
			out = append(out, SecretFinding{Scope: d.Scope, Server: d.Server, Field: "url"})
		}
	}
	return out
}

// redact returns a copy of defs with every secret-shaped value replaced, so a Report (JSON or text)
// never carries a credential. Run FindSecrets on the ORIGINAL defs first — a redacted value no
// longer looks like a secret, so scanning would find nothing.
func redact(defs []Def) []Def {
	out := make([]Def, len(defs))
	for i, d := range defs {
		d.D.Env = redactMap(d.D.Env)
		d.D.Headers = redactMap(d.D.Headers)
		if d.D.URL != "" {
			d.D.URL = queryParamSecret.ReplaceAllString(d.D.URL, "${1}REDACTED")
		}
		out[i] = d
	}
	return out
}

func redactMap(m map[string]string) map[string]string { return shapes.RedactMap(m) }

func scanMap(d Def, field string, m map[string]string) []SecretFinding {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []SecretFinding
	for _, k := range keys {
		if looksLikeSecret(m[k]) {
			out = append(out, SecretFinding{Scope: d.Scope, Server: d.Server, Field: field, Key: k})
		}
	}
	return out
}

// Report is the full doctor result: every definition plus the three findings sections.
type Report struct {
	Defs      []Def           `json:"defs"`
	Health    HealthResult    `json:"health"`
	Conflicts []Conflict      `json:"conflicts"`
	Secrets   []SecretFinding `json:"secrets"`
}

// ExitCode is 1 when any server failed to connect or any secret was found, else 0. A fail-open
// health skip (claude not on PATH) never contributes to the exit code on its own.
func (r Report) ExitCode() int {
	if r.Health.Failed > 0 || len(r.Secrets) > 0 {
		return 1
	}
	return 0
}

// Options configures one Doctor run; every path is read-only.
type Options struct {
	UserPath      string
	ProjectFiles  []string
	SettingsFiles []string
	Claude        string // resolved `claude` binary, "" if not found (fails OPEN)
	Home          string
	JSON          bool
}

// Doctor runs the full scan and writes either the human report or (Options.JSON) the Report as JSON.
func Doctor(w io.Writer, o Options) int {
	defs := CollectDefs(o.UserPath, o.ProjectFiles, o.SettingsFiles)
	secrets := FindSecrets(defs) // must run before redact() — a scrubbed value no longer looks like one
	safe := redact(defs)
	rep := Report{
		Defs:      safe,
		Health:    RunHealth(o.Claude),
		Conflicts: FindConflicts(safe, o.Home),
		Secrets:   secrets,
	}
	if o.JSON {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			fmt.Fprintln(w, "{}")
			return 1
		}
		fmt.Fprintln(w, string(b))
		return rep.ExitCode()
	}
	printReport(w, rep)
	return rep.ExitCode()
}

// printReport renders rep as the human-readable, section-per-check report.
func printReport(w io.Writer, rep Report) {
	fmt.Fprintln(w, "== MCP servers (all scopes) ==")
	if len(rep.Defs) == 0 {
		fmt.Fprintln(w, "  no MCP servers defined anywhere")
	}
	for _, d := range rep.Defs {
		fmt.Fprintf(w, "  [%s] %s: %s\n", d.Scope, d.Server, d.D.display())
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "== MCP server health ==")
	switch {
	case rep.Health.Skipped != "":
		fmt.Fprintln(w, "  live health:", rep.Health.Skipped)
	default:
		for _, ln := range rep.Health.Lines {
			fmt.Fprintln(w, "  "+ln)
		}
		if rep.Health.Error != "" {
			fmt.Fprintf(w, "  ⚠ `claude mcp list` failed: %s\n", rep.Health.Error)
		} else if rep.Health.Failed > 0 {
			fmt.Fprintf(w, "  ⚠ %d server(s) failed to connect\n", rep.Health.Failed)
			fmt.Fprintln(w, "  note: npx-launched servers can false-negative on a cold-start timeout")
		} else {
			fmt.Fprintf(w, "  ✓ all reachable servers connected (%d)\n", rep.Health.Connected)
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "== scope conflicts (same server in >1 scope) ==")
	if len(rep.Conflicts) == 0 {
		fmt.Fprintln(w, "  ✓ no server defined in more than one scope")
	}
	realConflicts := 0
	for _, c := range rep.Conflicts {
		for _, r := range c.Rows {
			fmt.Fprintf(w, "      [%s] %s\n", r.Scope, r.D.display())
		}
		if c.Real {
			fmt.Fprintf(w, "  ⚠ server '%s' has DIFFERENT commands across scopes (real conflict):\n", c.Server)
			fmt.Fprintf(w, "      → keep one endpoint: claude mcp remove %s -s <scope>\n", c.Server)
			realConflicts++
		} else {
			fmt.Fprintf(w, "    · '%s' appears in multiple scopes but with the SAME command — intentional layering, benign.\n", c.Server)
		}
	}
	if len(rep.Conflicts) > 0 && realConflicts == 0 {
		fmt.Fprintln(w, "  ✓ no conflicting endpoints (only benign same-command layering)")
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "== inline credential scan (MCP env/headers/url) ==")
	if len(rep.Secrets) == 0 {
		fmt.Fprintln(w, "  ✓ no inline secrets found")
	}
	for _, f := range rep.Secrets {
		switch f.Field {
		case "url":
			fmt.Fprintf(w, "  ⚠ [%s] server '%s' url has a plaintext token/key query param\n", f.Scope, f.Server)
		default:
			fmt.Fprintf(w, "  ⚠ [%s] server '%s' %s %s holds an inline secret (use ${ENV} or a token helper)\n", f.Scope, f.Server, f.Field, f.Key)
		}
	}
}
