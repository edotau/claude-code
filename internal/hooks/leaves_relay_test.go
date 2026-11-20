package hooks

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

func TestRelaySyntax(t *testing.T) {
	for _, cmd := range []string{
		"claude-code agents ask x > /some/file.go",
		"claude-code agents ask $(printf x)",
		"claude-code agents ask `printf x`",
		"claude-code agents ask <(printf x)",
		"claude-code agents ask x 2>&1 > /some/file",
	} {
		sess := relayTx(t)
		if r := Handback(bashHook("PreToolUse", sess, "a1", "t2", cmd, "")); r.Decision != core.Block {
			t.Errorf("HIGH1 %q allowed: %+v", cmd, r)
		}
	}
	r := Handback(bashHook("PreToolUse", relayTx(t), "a1", "t2", "claude-code agents ask $(printf x)", ""))
	if want := "refused `claude-code agents ask $(printf x)`: contains `$` or a backtick"; !strings.Contains(r.Reason, want) {
		t.Errorf("reason %q, want it to contain %q", r.Reason, want)
	}
}

// A relay may run read-only commands and go gates; edits and other runners stay refused; cd, test -f and 2>&1 pass.
func TestRelayReadOnlyAndGates(t *testing.T) {
	cases := map[string]bool{
		"rtk grep -n foo internal/hooks/x.go": true, "grep -rn foo internal": true, "git diff": true,
		"git -C /Users/edotau/.claude diff --stat": true, "git show 1ca08500 --stat": true,
		"go test -count=1 -run TestCancelDetectsPIDReuse ./internal/agent/": true, "rtk go test ./internal/hooks/": true,
		"go test ./... | tail -20": true, "go build ./... && go vet ./internal/hooks/": true, "gofmt -l internal/hooks/x.go": true,
		"sed -i s/a/b/ f": false, "echo x > f": false, "git commit -m x": false, "git add f": false, "git stash": false,
		"go run ./cmd/x": false, "go test -exec ./x ./...": false, "go build -o /tmp/x ./cmd/claude-code": false,
		"make test": false, "rm f": false, `python3 -c "print(1)"`: false, `bash -c "ls"`: false, "gofmt -w f.go": false,
		"cd /x && grep y z": true, "> /tmp/relay-pwn cd /tmp": false, `X="$(touch /tmp/relay-pwn)" cd /tmp`: false, "cd -- /x": false,
		"cd -P /x": false, "cd /x ; touch /tmp/p": false, "cd /x | tee /tmp/f": false, "cat <(touch /tmp/p)": false, "test -f x -a -w y": false,
		"ls 2>&1>/tmp/f": false, "ls 2>&1 >/tmp/f": false, "FOO=1 cd /x": false, "cd /x 2>&1": true, "cd /x\ntouch /tmp/p": false, "cd /x && sed -i s/a/b/ f": false, "test -f /x && echo ok || echo no": true,
		"claude-code agents ls 2>&1 | head -8": true, "go generate ./...": false, "GOFLAGS=-toolexec=x go test ./...": false,
		`claude-code worktree commit -m "fix: x" a.txt`: false, `rtk claude-code worktree commit -m "fix: x" a.txt`: false,
		`/usr/local/bin/claude-code --remote h worktree commit -m x a.txt`: false, `env claude-code worktree commit -m x a.txt`: false,
		"claude-code agents ask --agent codex x": true, "claude-code agents status 20261116T120000.000-codex": true,
		"claude-code worktree list": true, "claude-code agents diff --check --path a.go": true,
	}
	for cmd, want := range cases {
		r := Handback(bashHook("PreToolUse", relayTx(t), "a1", "t2", cmd, ""))
		if got := r.Decision != core.Block; got != want {
			t.Errorf("%q allowed=%v, want %v: %s", cmd, got, want, r.Reason)
		}
	}
}

// A relay's go gate is an explicit shape: packages and a short flag list, nothing that writes or runs a program.
func TestRelayGoGateShapes(t *testing.T) {
	for cmd, want := range relayGoCases {
		r := Handback(bashHook("PreToolUse", relayTx(t), "a1", "t2", cmd, ""))
		if got := r.Decision != core.Block; got != want {
			t.Errorf("%q allowed=%v, want %v: %s", cmd, got, want, r.Reason)
		}
	}
	t.Setenv("GOFLAGS", "-mod=readonly -toolexec=/x/y")
	r := Handback(bashHook("PreToolUse", relayTx(t), "a1", "t2", "go test ./...", ""))
	if r.Decision != core.Block || !strings.Contains(r.Reason, "inherited GOFLAGS") || !strings.Contains(r.Reason, "go build ./...") {
		t.Errorf("inherited GOFLAGS: %+v, want a block naming it and the allowed shapes", r)
	}
	t.Setenv("GOFLAGS", "-mod=readonly")
	if r := Handback(bashHook("PreToolUse", relayTx(t), "a1", "t2", "go test ./...", "")); r.Decision == core.Block {
		t.Errorf("harmless GOFLAGS: %+v, want allowed", r)
	}
}

// A status echo after a relay's ask is dropped by a rewrite, not refused; an echo of anything else stays refused.
func TestRelayDropsStatusEcho(t *testing.T) {
	ask := "claude-code agents ask --agent codex x - < /tmp/brief.md"
	for echo, want := range map[string]bool{`echo "exit=$?"`: true, "echo $?": true, `echo "rc=$?"`: true,
		`echo "$(id)"`: false, `echo $? > /tmp/f`: false, `echo $HOME`: false} {
		r := Handback(bashHook("PreToolUse", relayTx(t), "a1", "t2", ask+"; "+echo, ""))
		if got := r.Decision != core.Block && r.UpdatedInput != nil; got != want {
			t.Errorf("%q rewritten=%v, want %v: %+v", echo, got, want, r)
		} else if want && r.UpdatedInput["command"] != ask {
			t.Errorf("%q command %q, want %q", echo, r.UpdatedInput["command"], ask)
		}
	}
}
