package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestSafetyMessageCapsCommandOnRuneBoundary: 300 multibyte runes echo as 160 runes plus "…"; a short one stays whole.
func TestSafetyMessageCapsCommandOnRuneBoundary(t *testing.T) {
	msg := func(cmd string) string {
		in := ParseInputFor(strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"`+cmd+`"}}`), AgentClaude)
		return SafetyMessage(in, Block, "what", "", "hint")
	}
	long := msg(strings.Repeat("é", 300))
	if want := "  command: " + strings.Repeat("é", 160) + "…\n"; !strings.Contains(long, want) || !utf8.ValidString(long) {
		t.Fatalf("long command not capped at 160 runes on a rune boundary: %q", long)
	}
	short := msg("ls -l")
	if !strings.Contains(short, "  command: ls -l\n") || strings.Contains(short, "…") {
		t.Fatalf("short command must appear whole: %q", short)
	}
}

func TestAutomatedDelivery(t *testing.T) {
	msg := "Another Claude session sent a message:\n<agent-message from=\"a1\">\n[Subagent hand-back] done\n</agent-message>"
	note := "<task-notification>\n<task-id>b1</task-id>\n</task-notification>"
	cases := map[string]bool{
		msg:  true,
		note: true,
		"<system-reminder>x</system-reminder>\n" + note:               true,
		"[SYSTEM NOTIFICATION - NOT USER INPUT]\nbackground job done": true,
		msg + "\n\nnow fix the deploy pipeline":                       false,
		"fix the deploy pipeline\n" + note:                            false,
		"fix the deploy pipeline":                                     false,
		"":                                                            false,
	}
	for prompt, want := range cases {
		if got := AutomatedDelivery(prompt); got != want {
			t.Errorf("AutomatedDelivery(%.40q) = %v, want %v", prompt, got, want)
		}
	}
}

// A key shows once per (kind, session); another kind or session starts fresh.
func TestSeenAddOncePerKindAndSession(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for i, c := range []struct {
		kind, session, key string
		want               bool
	}{
		{"hooks/a", "s1", "k", true}, {"hooks/a", "s1", "k", false}, {"hooks/a", "s1", "k2", true},
		{"hooks/b", "s1", "k", true}, {"hooks/a", "s2", "k", true}, {"hooks/a", "s1", "k", false},
	} {
		if got, err := SeenAdd(c.kind, c.session, c.key); err != nil || got != c.want {
			t.Fatalf("step %d SeenAdd(%q, %q, %q) = %v, %v; want %v", i, c.kind, c.session, c.key, got, err, c.want)
		}
	}
}

// A new session's first write prunes its kind's files idle past seven days and keeps recent ones.
func TestSeenAddPrunesIdleSessions(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stale, recent := SeenFile("hooks/a", "old"), SeenFile("hooks/a", "recent")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{stale, recent} {
		if err := os.WriteFile(f, []byte("k\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := SeenAdd("hooks/a", "new", "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale seen file survived the prune (err %v)", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("recent seen file was pruned: %v", err)
	}
}
