package desktop

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

func testRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".desktop-test-")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}
func options(root string) Options {
	return Options{Root: root, BaseURL: "http://127.0.0.1:8080/p/dev", Profile: "Gateway",
		HelperPath: "/opt/claude-code", HelperArgs: []string{"gateway", "token", "--profile", "dev"}, Models: []Model{{Name: "model-1", LabelOverride: "Model One"}},
		Headers: map[string]string{"X-Workspace": "secret-tenant"}}
}
func run(opts Options, w io.Writer) error {
	return configure(opts, w, fileOps{write: atomicWrite, remove: os.Remove}, func(p string) (string, error) { return p, nil })
}
func read(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func write(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
func object(t *testing.T, p string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(read(t, p), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestConfigureIdempotentRevert(t *testing.T) {
	for _, previous := range []string{`"previous"`, `null`, ""} {
		t.Run("previous="+previous, func(t *testing.T) {
			root := testRoot(t)
			opts := options(root)
			metaPath := filepath.Join(root, "configLibrary", "_meta.json")
			original := `{"unknown":{"future":true},"profiles":[{"id":"other","name":"Other","extra":42}]`
			if previous != "" {
				original += `,"appliedId":` + previous
			}
			original += "}"
			write(t, metaPath, original, 0o640)
			var out bytes.Buffer
			if err := run(opts, &out); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "secret-tenant") {
				t.Fatal("header leaked")
			}
			profilePath := filepath.Join(root, "configLibrary", ProfileID+".json")
			var p profile
			if err := json.Unmarshal(read(t, profilePath), &p); err != nil {
				t.Fatal(err)
			}
			if p.InferenceProvider != "gateway" || p.ModelDiscoveryEnabled || p.InferenceGatewayAuthScheme != "bearer" ||
				p.InferenceCredentialKind != "helper-script" || p.InferenceCredentialHelperTTL != 900 || !p.InferenceCredentialHelperSilent ||
				p.InferenceCredentialHelper != opts.HelperPath || !reflect.DeepEqual(p.InferenceCredentialHelperArgs, opts.HelperArgs) ||
				!reflect.DeepEqual(p.InferenceModels, opts.Models) ||
				p.InferenceCustomHeaders["X-Workspace"] != "secret-tenant" {
				t.Fatalf("unexpected profile: %+v", p)
			}
			statePath := filepath.Join(root, stateName)
			state := read(t, statePath)
			meta := read(t, metaPath)
			prof := read(t, profilePath)
			writes := 0
			spy := fileOps{write: func(p string, b []byte, m os.FileMode) error { writes++; return atomicWrite(p, b, m) }, remove: os.Remove}
			if err := configure(opts, io.Discard, spy, func(p string) (string, error) { return p, nil }); err != nil {
				t.Fatal(err)
			}
			if writes != 0 {
				t.Fatalf("idempotent configure wrote %d files", writes)
			}
			if !bytes.Equal(state, read(t, statePath)) || !bytes.Equal(meta, read(t, metaPath)) || !bytes.Equal(prof, read(t, profilePath)) {
				t.Fatal("idempotence failed")
			}
			opts.Profile = "New name"
			opts.Models = []Model{{Name: "model-2"}}
			if err := run(opts, io.Discard); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(state, read(t, statePath)) {
				t.Fatal("original saved state overwritten")
			}
			m := object(t, metaPath)
			var unknown map[string]bool
			if json.Unmarshal(m["unknown"], &unknown) != nil || !unknown["future"] {
				t.Fatal("unknown metadata lost")
			}
			entries, err := profileEntries(m)
			if err != nil || len(entries) != 2 {
				t.Fatalf("entries %s, %v", m["profiles"], err)
			}
			var first map[string]any
			_ = json.Unmarshal(entries[0], &first)
			if first["extra"] != float64(42) {
				t.Fatal("unknown profile fields lost")
			}
			opts.Revert = true
			opts.BaseURL = ""
			opts.HelperPath = ""
			opts.Models = nil
			if err := run(opts, io.Discard); err != nil {
				t.Fatal(err)
			}
			restored := object(t, metaPath)
			raw, present := restored["appliedId"]
			if present != (previous != "") || (present && string(raw) != previous) {
				t.Fatalf("restored appliedId %s, %v", raw, present)
			}
			entries, err = profileEntries(restored)
			if err != nil || len(entries) != 1 {
				t.Fatal("unrelated entries not preserved")
			}
			for _, p := range []string{profilePath, statePath} {
				if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("not removed: %s", p)
				}
			}
			fi, err := os.Stat(metaPath)
			if err != nil || fi.Mode().Perm() != 0o640 {
				t.Fatal("metadata mode lost")
			}
			if err := run(opts, io.Discard); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDryRunNoWritesOrSecrets(t *testing.T) {
	root := testRoot(t)
	opts := options(root)
	opts.DryRun = true
	opts.Headers["X-Quoted"] = "sensitive\"value"
	var out bytes.Buffer
	seen := []string{}
	err := configure(opts, &out, fileOps{write: func(string, []byte, os.FileMode) error { t.Fatal("dry-run write"); return nil },
		remove: func(string) error { t.Fatal("dry-run removal"); return nil }}, func(p string) (string, error) { seen = append(seen, p); return `C:\Desktop\` + filepath.Base(p), nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "secret-tenant") || strings.Contains(out.String(), "sensitive") || !strings.Contains(out.String(), "[REDACTED]") {
		t.Fatal("dry-run leaked header values")
	}
	if len(seen) != 3 || !strings.Contains(out.String(), `C:\Desktop\_meta.json`) {
		t.Fatal("missing exact target paths")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("dry-run wrote: %v %v", entries, err)
	}
}

func TestNeverOpened(t *testing.T) {
	root := filepath.Join(testRoot(t), "missing")
	for _, dry := range []bool{false, true} {
		opts := options(root)
		opts.DryRun = dry
		if err := run(opts, io.Discard); err == nil || !strings.Contains(err.Error(), "open Desktop first") {
			t.Fatalf("got %v", err)
		}
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created missing root")
	}
}

func TestMalformedFilesRefused(t *testing.T) {
	cases := []struct{ name, meta, state string }{
		{"bad-json", `{`, ""}, {"null", `null`, ""}, {"array", `[]`, ""},
		{"lost-state", `{"appliedId":"` + ProfileID + `"}`, ""},
		{"profiles-null", `{"profiles":null}`, ""}, {"profiles-object", `{"profiles":{}}`, ""},
		{"bad-entry", `{"profiles":[null]}`, ""}, {"bad-id", `{"appliedId":12}`, ""},
		{"duplicate", `{"appliedId":"a","appliedId":"b"}`, ""},
		{"nested-duplicate", `{"future":{"a":1,"a":2}}`, ""},
		{"bad-state", `{}`, `{}`}, {"null-state", `{}`, `null`},
		{"newer-state", `{}`, `{"schema":2,"profileId":"` + ProfileID + `","previousPresent":false,"previousAppliedId":null}`},
		{"bad-saved-id", `{}`, `{"schema":1,"profileId":"` + ProfileID + `","previousPresent":true,"previousAppliedId":4}`},
		{"missing-state-presence", `{}`, `{"schema":1,"profileId":"` + ProfileID + `","previousAppliedId":null}`},
		{"null-state-presence", `{}`, `{"schema":1,"profileId":"` + ProfileID + `","previousPresent":null,"previousAppliedId":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			meta := filepath.Join(root, "configLibrary", "_meta.json")
			write(t, meta, tc.meta, 0o600)
			if tc.state != "" {
				write(t, filepath.Join(root, stateName), tc.state, 0o600)
			}
			for _, revert := range []bool{false, true} {
				opts := options(root)
				opts.Revert = revert
				if err := run(opts, io.Discard); err == nil {
					t.Fatal("accepted malformed file")
				}
			}
			if string(read(t, meta)) != tc.meta {
				t.Fatal("modified malformed metadata")
			}
			if _, err := os.Stat(filepath.Join(root, "configLibrary", ProfileID+".json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("created profile")
			}
		})
	}
}

func TestRollbackEveryMutation(t *testing.T) {
	for _, revert := range []bool{false, true} {
		for failAt := 1; failAt <= 3; failAt++ {
			t.Run(string(rune('0'+failAt))+map[bool]string{false: "write", true: "revert"}[revert], func(t *testing.T) {
				root := testRoot(t)
				opts := options(root)
				meta := filepath.Join(root, "configLibrary", "_meta.json")
				write(t, meta, `{"appliedId":"previous","profiles":[],"unknown":42}`, 0o640)
				profilePath := filepath.Join(root, "configLibrary", ProfileID+".json")
				write(t, profilePath, "{\"old\":true}\r\n", 0o644)
				statePath := filepath.Join(root, stateName)
				if revert {
					if err := run(opts, io.Discard); err != nil {
						t.Fatal(err)
					}
					opts.Revert = true
				}
				before := map[string]snapshot{}
				for _, p := range []string{meta, profilePath, statePath} {
					s, err := capture(p)
					if err != nil {
						t.Fatal(err)
					}
					before[p] = s
				}
				count := 0
				failure := errors.New("injected failure")
				ops := fileOps{write: func(p string, b []byte, m os.FileMode) error {
					if err := atomicWrite(p, b, m); err != nil {
						return err
					}
					count++
					if count == failAt {
						return failure
					}
					return nil
				},
					remove: func(p string) error {
						if err := os.Remove(p); err != nil {
							return err
						}
						count++
						if count == failAt {
							return failure
						}
						return nil
					}}
				err := configure(opts, io.Discard, ops, func(p string) (string, error) { return p, nil })
				if !errors.Is(err, failure) {
					t.Fatalf("expected injected failure, got %v", err)
				}
				for p, want := range before {
					got, err := capture(p)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("rollback did not restore %s: got %+v want %+v", p, got, want)
					}
				}
			})
		}
	}
}

func TestRollbackRemovesNewLibrary(t *testing.T) {
	root := testRoot(t)
	calls := 0
	ops := fileOps{write: func(p string, b []byte, m os.FileMode) error {
		calls++
		if calls == 3 {
			return errors.New("fail")
		}
		return atomicWrite(p, b, m)
	}, remove: os.Remove}
	if err := configure(options(root), io.Discard, ops, func(p string) (string, error) { return p, nil }); err == nil {
		t.Fatal("expected failure")
	}
	for _, p := range []string{"configLibrary", stateName} {
		if _, err := os.Stat(filepath.Join(root, p)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("left %s", p)
		}
	}
}

func TestDeduplicateOwnedEntry(t *testing.T) {
	root := testRoot(t)
	p := filepath.Join(root, "configLibrary", "_meta.json")
	write(t, p, `{"profiles":[{"id":"`+ProfileID+`","name":"old"},{"id":"other"},{"id":"`+ProfileID+`","name":"duplicate"}]}`, 0o600)
	if err := run(options(root), io.Discard); err != nil {
		t.Fatal(err)
	}
	entries, err := profileEntries(object(t, p))
	if err != nil || len(entries) != 2 {
		t.Fatalf("%s %v", read(t, p), err)
	}
}

func TestWindowsResolutionAndHelpers(t *testing.T) {
	resolver := &paths.Resolver{Mounts: []paths.Mount{{Point: "/windows", Drive: "C", Root: "/", Kind: paths.KindDrvFS}}}
	query := func() (string, error) { return `C:\Users\A Person\AppData\Local`, nil }
	root, err := rootFor("linux", true, "/home/me", "", query, resolver.Resolve)
	if err != nil || root != "/windows/Users/A Person/AppData/Local/Claude-3p" {
		t.Fatalf("%s %v", root, err)
	}
	for _, bad := range []string{"", "%LOCALAPPDATA%", "relative", "/home/linux"} {
		if _, err := rootFor("linux", true, "/home/me", "", func() (string, error) { return bad, nil }, resolver.Resolve); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if _, err := rootFor("linux", true, "/home/me", "", query, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("unmapped Windows path accepted")
	}
	if _, err := rootFor("linux", true, "/home/me", "", func() (string, error) { return "", errors.New("cmd.exe failed") }, resolver.Resolve); err == nil {
		t.Fatal("fell back to Linux")
	}
	for _, tc := range []struct{ goos, local, want string }{{"darwin", "", "/home/me/Library/Application Support/Claude-3p"}, {"linux", "", "/home/me/.config/Claude-3p"}, {"windows", `C:\Users\Me\AppData\Local`, `C:\Users\Me\AppData\Local\Claude-3p`}} {
		got, err := rootFor(tc.goos, false, "/home/me", tc.local, query, resolver.Resolve)
		if err != nil || got != tc.want {
			t.Fatalf("%s: %s %v", tc.goos, got, err)
		}
	}
	path, args, win, err := helperFor("linux", true, "/home/a b/bin/claude-code", "Ubuntu Work", "/home/a b/.claude", "dev")
	wantArgs := []string{"--distribution", "Ubuntu Work", "--exec", "/usr/bin/env", "CLAUDE_CONFIG_DIR=/home/a b/.claude",
		"/home/a b/bin/claude-code", "gateway", "token", "--profile", "dev"}
	if err != nil || path != wslExe || !win || !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("WSL helper: %s %q %v %v", path, args, win, err)
	}
	if _, _, _, err := helperFor("linux", true, "/bin/claude-code", "", "", "dev"); err == nil {
		t.Fatal("missing distro accepted")
	}
	if _, _, _, err := helperFor("linux", true, "claude-code", "Ubuntu", "", "dev"); err == nil {
		t.Fatal("relative executable accepted")
	}
	path, args, win, err = helperFor("darwin", false, "/opt/claude-code", "", "/Users/me/Claude Config", "dev")
	if err != nil || win || path != "/usr/bin/env" || args[0] != "CLAUDE_CONFIG_DIR=/Users/me/Claude Config" || args[1] != "/opt/claude-code" {
		t.Fatalf("Mac config directory not preserved: %s %q %v", path, args, err)
	}
	path, _, win, err = helperFor("windows", false, `C:\Program Files\claude-code.exe`, "", "", "dev")
	if err != nil || !win || path != `C:\Program Files\claude-code.exe` {
		t.Fatalf("%s %v", path, err)
	}
}

func TestSchemasRegistered(t *testing.T) {
	found := map[string]bool{}
	for _, f := range schemas.All() {
		if f.Owner == "internal/desktop" {
			found[f.Name] = true
		}
	}
	for _, name := range []string{"desktop-state", "desktop-profile", "desktop-meta"} {
		if !found[name] {
			t.Fatalf("missing %s", name)
		}
	}
	var s savedState
	raw := marshal(savedState{Schema: 1, ProfileID: ProfileID, PreviousAppliedID: json.RawMessage("null")})
	if _, err := schemas.Decode(desktopStateFormat, raw, &s); err != nil {
		t.Fatal(err)
	}
}

func TestRevertKeepsForeignAppliedAndBacksUpMeta(t *testing.T) {
	root := testRoot(t)
	meta := filepath.Join(root, "configLibrary", "_meta.json")
	write(t, meta, `{"appliedId":"mine","profiles":[]}`, 0o600)
	opts := options(root)
	if err := run(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	if string(read(t, meta+".bak")) != `{"appliedId":"mine","profiles":[]}` {
		t.Fatal("meta not backed up")
	}
	m := object(t, meta)
	m["appliedId"] = json.RawMessage(`"someone-else"`)
	write(t, meta, string(marshal(m)), 0o600)
	opts.Revert = true
	if err := run(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	if string(object(t, meta)["appliedId"]) != `"someone-else"` {
		t.Fatalf("foreign appliedId overwritten: %s", object(t, meta)["appliedId"])
	}
}

func TestEmptyRootIsAccepted(t *testing.T) {
	root := testRoot(t)
	if err := run(options(root), io.Discard); err != nil {
		t.Fatalf("empty Claude-3p dir refused: %v", err)
	}
}

func TestNativeWindowsHelperCarriesConfigDir(t *testing.T) {
	_, args, _, err := helperFor("windows", false, `C:\bin\cc.exe`, "", `C:\Users\Me\.claude`, "dev")
	if err != nil || !reflect.DeepEqual(args[len(args)-2:], []string{"--config-dir", `C:\Users\Me\.claude`}) {
		t.Fatalf("%q %v", args, err)
	}
}

func TestRevertBacksUpMetaEvenWhenCreatedByUs(t *testing.T) {
	root := testRoot(t)
	opts := options(root)
	if err := run(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	applied := read(t, filepath.Join(root, "configLibrary", "_meta.json"))
	opts.Revert = true
	if err := run(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(root, "configLibrary", "_meta.json.revert.bak")); string(got) != string(applied) {
		t.Fatalf("revert backup = %s", got)
	}
}

func TestRunningDesktopWarnsAndIsNotKilled(t *testing.T) {
	old := desktopRunning
	t.Cleanup(func() { desktopRunning = old })
	desktopRunning = func() bool { return true }
	var out strings.Builder
	if err := run(options(testRoot(t)), &out); err != nil || !strings.Contains(out.String(), "restart Claude Desktop") {
		t.Fatalf("%v %s", err, out.String())
	}
}
