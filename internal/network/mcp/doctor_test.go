package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// write is a small t.Helper wrapper around os.WriteFile for fixture setup.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeBin writes an executable shell script named name into dir and returns its path.
func fakeBin(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		t.Skip("fakeBin requires a POSIX shell")
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCollectDefs: user, local, project, and settings scopes all land; duplicate project files dedupe.
func TestCollectDefs(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir()) // macOS: /var -> /private/var, and CollectDefs reports resolved paths
	if err != nil {
		t.Fatal(err)
	}
	userJSON := filepath.Join(dir, ".claude.json")
	write(t, userJSON, `{
		"mcpServers": {"github": {"command": "npx", "args": ["gh-mcp"]}},
		"projects": {"/repo": {"mcpServers": {"jenkins": {"command": "bash", "args": ["launch.sh"]}}}}
	}`)
	proj := filepath.Join(dir, ".mcp.json")
	write(t, proj, `{"mcpServers": {"github": {"command": "npx", "args": ["gh-mcp"]}}}`)
	settings := filepath.Join(dir, "settings.json")
	write(t, settings, `{"mcpServers": {"sentry": {"type": "http", "url": "https://sentry.example/mcp"}}}`)

	defs := CollectDefs(userJSON, []string{proj, proj}, []string{settings})
	if len(defs) != 4 {
		t.Fatalf("defs = %d, want 4 (%+v)", len(defs), defs)
	}
	scopes := map[string]bool{}
	for _, d := range defs {
		scopes[d.Scope] = true
	}
	if !scopes["user"] || !scopes["local:/repo"] || !scopes["project:"+proj] || !scopes["settings:"+settings] {
		t.Fatalf("scopes = %v", scopes)
	}
}

// TestCollectDefsSkipsUnreadableOrInvalid: a missing or malformed file is a clean state, not an error.
func TestCollectDefsSkipsUnreadableOrInvalid(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, ".mcp.json")
	write(t, bad, `{not json`)
	defs := CollectDefs(filepath.Join(dir, "missing.json"), []string{bad, filepath.Join(dir, "absent.json")}, nil)
	if len(defs) != 0 {
		t.Fatalf("defs = %+v, want none", defs)
	}
}

// TestFindConflicts: same resolved command across scopes is benign; different commands flag real.
func TestFindConflicts(t *testing.T) {
	home := "/home/u"
	benign := []Def{
		{"github", "user", serverDef{Command: "${HOME}/mcp/launch.sh"}},
		{"github", "project:/x/.mcp.json", serverDef{Command: "/home/u/mcp/launch.sh"}},
	}
	conflicts := FindConflicts(benign, home)
	if len(conflicts) != 1 || conflicts[0].Real {
		t.Fatalf("benign layering flagged real: %+v", conflicts)
	}

	real := []Def{
		{"github", "user", serverDef{Command: "npx", Args: []string{"gh-mcp"}}},
		{"github", "project:/x/.mcp.json", serverDef{Command: "docker", Args: []string{"run", "gh"}}},
	}
	conflicts = FindConflicts(real, home)
	if len(conflicts) != 1 || !conflicts[0].Real {
		t.Fatalf("real conflict not flagged: %+v", conflicts)
	}

	single := []Def{{"solo", "user", serverDef{Command: "npx"}}}
	if c := FindConflicts(single, home); len(c) != 0 {
		t.Fatalf("single-scope server flagged: %+v", c)
	}
}

// TestFindSecrets: inline tokens in env/headers/url query strings flag; ${ENV} refs, file paths, and
// placeholders stay clean.
func TestFindSecrets(t *testing.T) {
	defs := []Def{
		{"databricks", "user", serverDef{Env: map[string]string{"DATABRICKS_TOKEN": "dapi0123456789abcdef"}}},
		{"github", "user", serverDef{Env: map[string]string{"GITHUB_TOKEN": "${GITHUB_PERSONAL_ACCESS_TOKEN}", "CA": "/etc/ssl/ca.pem"}}},
		{"openai", "user", serverDef{Env: map[string]string{"KEY": "sk-abcdefghijklmnopqrstuvwxyz0123456789ABCD"}}},
		{"slack", "user", serverDef{Env: map[string]string{"TOKEN": "xoxb-" + "1234567890" + "-abcdefghijklmnop"}}},
		{"remote", "project:/x/.mcp.json", serverDef{
			URL:     "https://example.com/sse?token=abcdef0123456789",
			Headers: map[string]string{"Authorization": "Bearer aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		}},
		{"placeholder", "user", serverDef{Env: map[string]string{"KEY": "YOUR_API_KEY_HERE"}}},
	}
	findings := FindSecrets(defs)
	if len(findings) != 5 {
		t.Fatalf("findings = %d, want 5 (dapi, sk-, xoxb-, url token=, header Bearer): %+v", len(findings), findings)
	}
	fields := map[string]int{}
	for _, f := range findings {
		fields[f.Field]++
	}
	if fields["env"] != 3 || fields["url"] != 1 || fields["headers"] != 1 {
		t.Errorf("by field = %+v, want env=3 url=1 headers=1", fields)
	}
}

// TestFindSecretsClean: a definition with no env/headers/url produces no findings.
func TestFindSecretsClean(t *testing.T) {
	defs := []Def{{"plain", "user", serverDef{Command: "npx", Args: []string{"server"}}}}
	if n := FindSecrets(defs); len(n) != 0 {
		t.Fatalf("findings = %+v, want none", n)
	}
}

// TestRunHealthSkipsOpenWithNoClaudeBinary: a missing `claude` binary fails OPEN — it is reported,
// not scored as a Failed server, and must not block the exit code on its own.
func TestRunHealthSkipsOpenWithNoClaudeBinary(t *testing.T) {
	res := RunHealth("")
	if res.Skipped == "" || res.Failed != 0 || res.Connected != 0 {
		t.Fatalf("RunHealth(\"\") = %+v, want a Skipped reason and zero counts", res)
	}
	rep := Report{Health: res}
	if rep.ExitCode() != 0 {
		t.Errorf("ExitCode = %d, want 0 (fail-open health alone must not block)", rep.ExitCode())
	}
}

// TestRunHealthCountsServers: connected and failed lines from `claude mcp list` are counted and kept.
func TestRunHealthCountsServers(t *testing.T) {
	dir := t.TempDir()
	claude := fakeBin(t, dir, "claude", "echo 'ctx7: ✔ connected'\necho 'bad: ✘ failed to connect'")
	res := RunHealth(claude)
	if res.Connected != 1 || res.Failed != 1 {
		t.Fatalf("RunHealth = %+v, want 1 connected 1 failed", res)
	}
	rep := Report{Health: res}
	if rep.ExitCode() != 1 {
		t.Errorf("ExitCode = %d, want 1 (a failed server)", rep.ExitCode())
	}
}

// TestRunHealthReportsListErrorEvenWithParsedLines: a `claude mcp list` that dies after printing a
// parseable line must still surface the run error instead of silently dropping it.
func TestRunHealthReportsListErrorEvenWithParsedLines(t *testing.T) {
	dir := t.TempDir()
	claude := fakeBin(t, dir, "claude", "echo 'ctx7: ✔ connected'\nexit 1")
	res := RunHealth(claude)
	if res.Error == "" {
		t.Errorf("RunHealth = %+v, want a non-empty Error (list command exited non-zero)", res)
	}
}

// TestRunHealthKillsHungListOnTimeout: a hung/cold-starting `claude mcp list` must be killed by the
// context, not block forever.
func TestRunHealthKillsHungListOnTimeout(t *testing.T) {
	orig := listTimeout
	listTimeout = 50 * time.Millisecond
	defer func() { listTimeout = orig }()

	dir := t.TempDir()
	claude := fakeBin(t, dir, "claude", "sleep 5")

	done := make(chan HealthResult, 1)
	go func() { done <- RunHealth(claude) }()

	select {
	case res := <-done:
		if res.Error == "" {
			t.Errorf("RunHealth = %+v, want a timeout Error", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunHealth did not return within 2s — the hung `claude mcp list` was not killed")
	}
}

// TestHealthCapturesListThroughAFileNotAPipe: stdout/stderr must be captured via a file, not a pipe,
// or a non-flushing CLI exit truncates the output and a dropped failure line reads as an all-clear.
func TestHealthCapturesListThroughAFileNotAPipe(t *testing.T) {
	dir := t.TempDir()
	claude := fakeBin(t, dir, "claude", `if [ -p /dev/stdout ]; then
  echo 'ctx7: ok'
else
  echo 'ctx7: ✔ connected'
  echo 'bad: ✘ failed to connect'
fi`)
	res := RunHealth(claude)
	if res.Failed != 1 {
		t.Errorf("RunHealth = %+v, want 1 failed — a piped capture lost the failure line", res)
	}
}

// TestRedactScrubsEnvHeadersAndURL: redact() must blank every secret-shaped value it is handed —
// the Report embeds Defs verbatim, so a missed field leaks a credential straight into --json/text.
func TestRedactScrubsEnvHeadersAndURL(t *testing.T) {
	defs := []Def{{"s", "user", serverDef{
		Env:     map[string]string{"TOKEN": "dapi0123456789abcdef", "CA": "/etc/ssl/ca.pem"},
		Headers: map[string]string{"Authorization": "Bearer aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		URL:     "https://example.com/sse?token=abcdef0123456789&other=1",
	}}}
	safe := redact(defs)[0].D
	if safe.Env["TOKEN"] != "REDACTED" || safe.Env["CA"] != "/etc/ssl/ca.pem" {
		t.Errorf("env after redact = %+v", safe.Env)
	}
	if safe.Headers["Authorization"] != "REDACTED" {
		t.Errorf("headers after redact = %+v", safe.Headers)
	}
	if strings.Contains(safe.URL, "abcdef0123456789") || !strings.Contains(safe.URL, "token=REDACTED") || !strings.Contains(safe.URL, "other=1") {
		t.Errorf("url after redact = %q", safe.URL)
	}
}

// TestDoctorJSONRoundTrips: --json mode emits a Report that decodes back cleanly.
func TestDoctorJSONRoundTrips(t *testing.T) {
	dir := t.TempDir()
	userJSON := filepath.Join(dir, ".claude.json")
	write(t, userJSON, `{"mcpServers": {"github": {"command": "npx", "args": ["gh-mcp"], "env": {"TOKEN": "sk-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}}`)

	var out strings.Builder
	code := Doctor(&out, Options{UserPath: userJSON, Home: dir, JSON: true})
	if code != 1 {
		t.Fatalf("Doctor exit = %d, want 1 (inline secret)", code)
	}
	var rep Report
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, out.String())
	}
	if len(rep.Secrets) != 1 || len(rep.Defs) != 1 {
		t.Fatalf("decoded report = %+v", rep)
	}
	if strings.Contains(out.String(), "sk-aaaa") {
		t.Error("JSON report echoed the secret value, not just its location")
	}
}

// TestDoctorTextReportNeverEchoesSecretValues: the human report names scope/server/field, never the
// flagged value itself.
func TestDoctorTextReportNeverEchoesSecretValues(t *testing.T) {
	dir := t.TempDir()
	userJSON := filepath.Join(dir, ".claude.json")
	write(t, userJSON, `{"mcpServers": {"github": {"command": "npx", "env": {"TOKEN": "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}}`)

	var out strings.Builder
	code := Doctor(&out, Options{UserPath: userJSON, Home: dir})
	if code != 1 {
		t.Fatalf("Doctor exit = %d, want 1", code)
	}
	if strings.Contains(out.String(), "ghp_aaaa") {
		t.Fatal("text report echoed the secret value")
	}
	if !strings.Contains(out.String(), "holds an inline secret") {
		t.Errorf("text report missing the secret finding:\n%s", out.String())
	}
}

// A serverDef marshals redacted at the type, so no JSON path can forget redact(); has_secrets says one existed.
func TestServerDefMarshalRedactsAtTheType(t *testing.T) {
	tok := "ghp" + "_abcdefghijklmnopqrstuvwxyz0123"
	d := Def{Server: "x", Scope: "user", D: serverDef{URL: "https://h/?token=" + tok, Env: map[string]string{"T": tok}}}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), tok) || !strings.Contains(string(b), `"has_secrets":true`) {
		t.Errorf("marshal leaked or unflagged: %s", b)
	}
	if b, _ := json.Marshal(Def{D: serverDef{Command: "npx"}}); strings.Contains(string(b), "has_secrets") {
		t.Errorf("clean def must not carry has_secrets: %s", b)
	}
}
