package cli

import (
	"strings"
	"testing"
)

func TestNetworkBareAndHelpPrintUsageExitTwo(t *testing.T) {
	for _, args := range [][]string{nil, {"-h"}, {"--help"}} {
		code, _, stderr := captureOutput(t, func() int { return cmdNetwork(args) })
		if code != 2 {
			t.Errorf("network %v: exit %d, want 2", args, code)
		}
		for _, sub := range []string{"tailscale", "browser", "websearch", "mcp", "page", "view", "vscode"} {
			if !strings.Contains(stderr, sub) {
				t.Errorf("network %v: help lacks %q: %q", args, sub, stderr)
			}
		}
	}
}

func TestNetworkUnknownSubverb(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return cmdNetwork([]string{"bogus"}) })
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, `unknown subcommand "bogus"`) || !strings.Contains(stderr, "tailscale|browser|websearch|mcp|page|view|vscode") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestNetworkOldSubverbsRejected(t *testing.T) {
	for _, old := range []string{"web", "doctor", "status", "connect", "browsers"} {
		if code, _, _ := captureOutput(t, func() int { return cmdNetwork([]string{old}) }); code != 1 {
			t.Errorf("network %s: exit %d, want 1", old, code)
		}
	}
}
