// mcphealth is a read-only diagnostic for Claude Code's MCP config surface: it lists every
// defined server, runs `claude mcp list` for live connect health, flags a server name defined with
// different commands/urls across scopes, and flags inline plaintext secrets.

package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
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
	return json.Marshal(struct {
		plain
		HasSecrets bool `json:"has_secrets,omitempty"`
	}{plain(d.redacted()), textutil.AnySecret(d.Env) || textutil.AnySecret(d.Headers) || (d.URL != "" && queryParamSecret.MatchString(d.URL))})
}

// redacted returns a copy of d with every secret-shaped env, header, arg and URL query value replaced.
func (d serverDef) redacted() serverDef {
	d.Env, d.Headers, d.Args = textutil.RedactSecretMap(d.Env), textutil.RedactSecretMap(d.Headers), redactArgs(d.Args)
	if d.URL != "" {
		d.URL = queryParamSecret.ReplaceAllString(d.URL, "${1}REDACTED")
	}
	return d
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

// collectDefs reads mcpServers from the user config, project .mcp.json and settings files; bad files are noted.
func collectDefs(userPath string, projectFiles, settingsFiles []string) ([]Def, []string) {
	var out []Def
	var bad []string
	var top struct {
		MCPServers map[string]serverDef `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers map[string]serverDef `json:"mcpServers"`
		} `json:"projects"`
	}
	if loadConfig(userPath, &top, &bad) {
		for name, d := range top.MCPServers {
			out = append(out, Def{name, "user", d})
		}
		for proj, p := range top.Projects {
			for name, d := range p.MCPServers {
				out = append(out, Def{name, "local:" + proj, d})
			}
		}
	}
	out = append(out, readFlatScoped(projectFiles, "project:", &bad)...)
	out = append(out, readFlatScoped(settingsFiles, "settings:", &bad)...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Scope < out[j].Scope
	})
	return out, bad
}

// loadConfig decodes path into doc; a file that exists but cannot be read or parsed is noted in bad.
func loadConfig(path string, doc any, bad *[]string) bool {
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, doc)
	}
	if err == nil {
		return true
	}
	if !errors.Is(err, fs.ErrNotExist) {
		*bad = append(*bad, path+": "+err.Error())
	}
	return false
}

// readFlatScoped reads a flat {"mcpServers": {...}} file (.mcp.json, settings.json), deduped by
// resolved path, and tags each server with prefix+resolvedPath as its scope.
func readFlatScoped(files []string, prefix string, bad *[]string) []Def {
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
		if !loadConfig(rp, &doc, bad) {
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
// (measured: 10/12 pipe reads short, 0/12 file reads), and a dropped failure line reads
// as an all-clear.
func mcpList(claude string) ([]byte, error) {
	f, err := os.CreateTemp("", "claude-mcp-list-*.txt")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
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

// healthResult is the outcome of a live `claude mcp list` connect check.
type healthResult struct {
	Lines     []string `json:"lines,omitempty"`
	Connected int      `json:"connected"`
	Failed    int      `json:"failed"`
	Error     string   `json:"error,omitempty"`
	Skipped   string   `json:"skipped,omitempty"` // fail-open reason: live health was not run
}

// runHealth runs `claude mcp list` and counts connected vs failed servers. claude=="" fails OPEN
// (reported, not treated as a Failed server) since there is nothing to connect with.
func runHealth(claude string) healthResult {
	if claude == "" {
		return healthResult{Skipped: "claude not on PATH"}
	}
	out, listErr := mcpList(claude)
	var res healthResult
	for ln := range strings.SplitSeq(string(out), "\n") {
		switch {
		case strings.Contains(ln, "✔"):
			res.Connected++
		case strings.Contains(ln, "✘"):
			res.Failed++
		default:
			continue
		}
		res.Lines = append(res.Lines, scrub(strings.TrimSpace(ln)))
	}
	if listErr != nil {
		res.Error = scrub(listErr.Error())
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

// findConflicts flags same-name defs that disagree; the comparison runs on the originals so a secret-only diff counts.
func findConflicts(defs []Def, home string) []Conflict {
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
			cmd = expandArgsHome(cmd)
			resolved[cmd] = true
		}
		out = append(out, Conflict{Server: name, Rows: redact(rows), Real: len(resolved) > 1})
	}
	return out
}

// expandArgsHome expands a leading ~ or ~/ in each whitespace-separated argument of cmd.
func expandArgsHome(cmd string) string {
	args := strings.Fields(cmd)
	for i, a := range args {
		args[i] = paths.ExpandHome(a)
	}
	return strings.Join(args, " ")
}

// SecretFinding is one inline plaintext credential flagged in a server definition.
type SecretFinding struct {
	Scope  string `json:"scope"`
	Server string `json:"server"`
	Field  string `json:"field"` // env | headers | url
	Key    string `json:"key,omitempty"`
}

// queryParamSecret matches a plaintext *token=/*key= query parameter in a URL; group 1 keeps the
// separator and param name so redact() can replace just the value.
var queryParamSecret = regexp.MustCompile(`(?i)([?&][\w-]*(?:token|key)=)[^&\s]+`)

// findSecrets scans every server's env, headers, and url for inline plaintext credentials.
func findSecrets(defs []Def) []SecretFinding {
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
// never carries a credential. Run findSecrets on the ORIGINAL defs first — a redacted value no
// longer looks like a secret, so scanning would find nothing.
func redact(defs []Def) []Def {
	out := make([]Def, len(defs))
	for i, d := range defs {
		d.D = d.D.redacted()
		out[i] = d
	}
	return out
}

// scrub masks credential-shaped words and token/key query values in free text.
func scrub(s string) string {
	return queryParamSecret.ReplaceAllString(textutil.RedactSecrets(s), "${1}REDACTED")
}

// redactArgs returns a copy of args with credential-shaped words masked.
func redactArgs(args []string) []string {
	if args == nil {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = scrub(a)
	}
	return out
}

func scanMap(d Def, field string, m map[string]string) []SecretFinding {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []SecretFinding
	for _, k := range keys {
		if textutil.LooksSecret(m[k]) {
			out = append(out, SecretFinding{Scope: d.Scope, Server: d.Server, Field: field, Key: k})
		}
	}
	return out
}

// Report is the full doctor result: every definition plus the three findings sections.
type Report struct {
	Defs      []Def           `json:"defs"`
	Health    healthResult    `json:"health"`
	Conflicts []Conflict      `json:"conflicts"`
	Secrets   []SecretFinding `json:"secrets"`
	Unscanned []string        `json:"unscanned,omitempty"` // config files that exist but could not be read or parsed
}

// ExitCode is 1 when a server failed, `claude mcp list` errored, a secret was found, or a config file was unscanned, else 0. A fail-open
// health skip (claude not on PATH) never contributes to the exit code on its own.
func (r Report) ExitCode() int {
	if r.Health.Failed > 0 || r.Health.Error != "" || len(r.Secrets) > 0 || len(r.Unscanned) > 0 {
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

// Summary is the one-row `doctor` view of the config scan, without the ~1s live `claude mcp list` (doctor --mcp runs it).
func Summary(o Options) (ok bool, detail string) {
	rep := configReport(o)
	detail = fmt.Sprintf("%d defined, %d secrets, %d unscanned; doctor --mcp for live health",
		len(rep.Defs), len(rep.Secrets), len(rep.Unscanned))
	return rep.ExitCode() == 0, detail
}

// Doctor runs the full scan and writes either the human report or (Options.JSON) the Report as JSON.
func Doctor(w io.Writer, o Options) int {
	rep := buildReport(o)
	ew := &errWriter{w: w}
	if o.JSON {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			fmt.Fprintln(ew, "{}")
			return 1
		}
		fmt.Fprintln(ew, string(b))
	} else {
		printReport(ew, rep)
	}
	if ew.err != nil {
		fmt.Fprintln(os.Stderr, "doctor --mcp: could not write report:", ew.err)
		return 1
	}
	return rep.ExitCode()
}

// errWriter keeps the first write error, so a truncated report is detected once Doctor finishes.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, err
}

// printReport renders rep as the human-readable, section-per-check report.
func printReport(w io.Writer, rep Report) {
	printDefsSection(w, rep)
	fmt.Fprintln(w)
	printHealthSection(w, rep.Health)
	fmt.Fprintln(w)
	printConflictsSection(w, rep.Conflicts)
	fmt.Fprintln(w)
	printSecretsSection(w, rep)
}

func printDefsSection(w io.Writer, rep Report) {
	fmt.Fprintln(w, "== MCP servers (all scopes) ==")
	if len(rep.Defs) == 0 {
		fmt.Fprintln(w, "  no MCP servers defined anywhere")
	}
	for _, d := range rep.Defs {
		fmt.Fprintf(w, "  [%s] %s: %s\n", d.Scope, d.Server, d.D.display())
	}
}

func printHealthSection(w io.Writer, h healthResult) {
	fmt.Fprintln(w, "== MCP server health ==")
	if h.Skipped != "" {
		fmt.Fprintln(w, "  live health:", h.Skipped)
		return
	}
	for _, ln := range h.Lines {
		fmt.Fprintln(w, "  "+ln)
	}
	switch {
	case h.Error != "":
		fmt.Fprintf(w, "  ⚠ `claude mcp list` failed: %s\n", h.Error)
	case h.Failed > 0:
		fmt.Fprintf(w, "  ⚠ %d server(s) failed to connect\n", h.Failed)
		fmt.Fprintln(w, "  note: npx-launched servers can false-negative on a cold-start timeout")
	default:
		fmt.Fprintf(w, "  ✓ all reachable servers connected (%d)\n", h.Connected)
	}
}

func printConflictsSection(w io.Writer, conflicts []Conflict) {
	fmt.Fprintln(w, "== scope conflicts (same server in >1 scope) ==")
	if len(conflicts) == 0 {
		fmt.Fprintln(w, "  ✓ no server defined in more than one scope")
	}
	realConflicts := 0
	for _, c := range conflicts {
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
	if len(conflicts) > 0 && realConflicts == 0 {
		fmt.Fprintln(w, "  ✓ no conflicting endpoints (only benign same-command layering)")
	}
}

func printSecretsSection(w io.Writer, rep Report) {
	fmt.Fprintln(w, "== inline credential scan (MCP env/headers/url) ==")
	if len(rep.Secrets) == 0 && len(rep.Unscanned) == 0 {
		fmt.Fprintln(w, "  ✓ no inline secrets found")
	}
	for _, u := range rep.Unscanned {
		fmt.Fprintf(w, "  ⚠ not scanned (unreadable or invalid JSON): %s\n", u)
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

// configReport collects every scope and finds inline secrets and conflicts; it runs no live health check.
func configReport(o Options) Report {
	defs, unscanned := collectDefs(o.UserPath, o.ProjectFiles, o.SettingsFiles)
	secrets := findSecrets(defs) // must run before redact() — a scrubbed value no longer looks like one
	return Report{
		Defs:      redact(defs),
		Conflicts: findConflicts(defs, o.Home),
		Secrets:   secrets,
		Unscanned: unscanned,
	}
}

// buildReport is configReport plus the live `claude mcp list` health.
func buildReport(o Options) Report {
	rep := configReport(o)
	rep.Health = runHealth(o.Claude)
	if len(rep.Defs) > 0 && rep.Health.Skipped == "" && rep.Health.Error == "" && rep.Health.Connected+rep.Health.Failed == 0 {
		rep.Health.Error = "no server status lines recognised in `claude mcp list` output"
	}
	return rep
}
