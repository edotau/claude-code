package vscode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinkExtensionCheckRejectsDeletedSource(t *testing.T) {
	home := extensionHome(t)
	extensions := t.TempDir()
	if _, err := LinkExtension(home, extensions, false); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(home, "vscode", "claude-code-agents")); err != nil {
		t.Fatal(err)
	}
	_, err := LinkExtension(home, extensions, true)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check of dangling matching link = %v, want missing-source error", err)
	}
}

func TestSyncEscapesTemplatePaths(t *testing.T) {
	hermeticVSCode(t)
	dir := t.TempDir()
	tmpl, dest := filepath.Join(dir, "template.json"), filepath.Join(dir, "settings.json")
	writeFile(t, tmpl, `{"home":"__CLAUDE_HOME__", "bin":"__CLAUDE_BIN__/codex"}`)
	home, bin := `/tmp/home"with\slash`, `/tmp/a"b/bin`
	if _, err := Sync(tmpl, dest, home, bin, false); err != nil {
		t.Fatalf("sync with quoted paths: %v", err)
	}
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("merged JSON: %v", err)
	}
	if got["home"] != home || got["bin"] != bin+"/codex" {
		t.Fatalf("rendered paths = %v, want home %q and bin %q", got, home, bin+"/codex")
	}
}

func TestNoTailscalePreservesMappingOnTeardownFailure(t *testing.T) {
	for _, funnel := range []bool{false, true} {
		t.Run(map[bool]string{false: "serve", true: "funnel"}[funnel], func(t *testing.T) {
			setup(t)
			fakeTailscaleFailure(t)
			prev := State{PID: os.Getpid(), Port: DefaultPort, Tailscale: true, Funnel: funnel, URL: "https://example.ts.net/"}
			res, err := applyTailscale(context.Background(), prev, Options{NoTailscale: true})
			if err == nil || !strings.Contains(err.Error(), "teardown denied") {
				t.Fatalf("teardown error = %v, want CLI failure", err)
			}
			if res.State != prev {
				t.Fatalf("returned state = %+v, want preserved %+v", res.State, prev)
			}
			saved, err := readState()
			if err != nil {
				t.Fatal(err)
			}
			prev.Schema = stateSchema
			if saved != prev {
				t.Fatalf("persisted state = %+v, want preserved %+v", saved, prev)
			}
		})
	}
}

func fakeTailscaleFailure(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tailscale"), []byte("#!/bin/sh\necho 'teardown denied' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

func TestTailscaleFallbackNotesMatchLoopbackBinding(t *testing.T) {
	for _, available := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing_cli", true: "unconfirmed_mapping"}[available], func(t *testing.T) {
			setup(t)
			bin := t.TempDir()
			t.Setenv("PATH", bin)
			want := "127.0.0.1 only"
			if available {
				if err := os.WriteFile(filepath.Join(bin, "tailscale"), []byte("#!/bin/sh\necho '{}'\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				want = "check `tailscale serve status`"
			}
			res, err := applyTailscale(context.Background(), State{Port: DefaultPort}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], want) {
				t.Fatalf("fallback notes = %q, want %q", res.Notes, want)
			}
		})
	}
}
