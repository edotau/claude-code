package dispatchhook

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory/memtest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"github.com/edotau/claude-code/internal/worktree"
	"github.com/edotau/claude-code/internal/worktree/worktreetest"
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

func TestRelayCleansOutputWrappers(t *testing.T) {
	for cmd, want := range map[string]string{
		"rtk claude-code agents ls":                "claude-code agents ls",
		"claude-code agents ls 2>&1":               "claude-code agents ls",
		"claude-code agents ls | tail -8":          "claude-code agents ls",
		"claude-code agents ls | head -12":         "claude-code agents ls",
		"rtk claude-code agents ls 2>&1 | tail -8": "claude-code agents ls",
		"rm x":           "",
		"rm x | tail -8": "",
		"rtk claude-code agents ls | tail -8; rm x": "",
	} {
		r := Handback(bashHook("PreToolUse", relayTx(t), "a1", "t2", cmd, ""))
		if want == "" {
			if r.Decision != core.Block || r.UpdatedInput != nil {
				t.Errorf("%q: want refusal, got %+v", cmd, r)
			}
			continue
		}
		if r.Decision == core.Block || r.UpdatedInput == nil || r.UpdatedInput["command"] != want {
			t.Errorf("%q: want rewrite to %q, got %+v", cmd, want, r)
		}
	}
	if r := Handback(bashHook("PreToolUse", relayTx(t), "a1", "t2", "git diff | tail -8", "")); r.UpdatedInput != nil {
		t.Errorf("non-claude command rewritten: %+v", r)
	}
}

// A held-job hint names the route as one --overlay object, the profile's provider from the registry.
func TestOverlayArgsCarriesTheWholeRoute(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"qa":{"preset":"databricks","host":"https://example.invalid","auth":{"type":"none"}}}}}`)
	want := `{"provider":"databricks","profile":"qa","model":"sonnet"}`
	if got := OverlayArgs("qa", "sonnet", "", "acct"); !slices.Equal(got, []string{"--overlay", want}) {
		t.Errorf("OverlayArgs(qa, sonnet) = %q, want --overlay %s", got, want)
	}
	pool := []string{"--overlay", `{"provider":"databricks","profile":"auto"}`}
	if got := OverlayArgs("databricks", "", "", ""); !slices.Equal(got, pool) {
		t.Errorf("OverlayArgs(databricks) = %q, want %q", got, pool)
	}
	if got := OverlayArgs("nope", "m", "", ""); !slices.Equal(got, []string{"--profile", "nope", "--overlay", `{"model":"m"}`}) {
		t.Errorf("OverlayArgs(unknown row) = %q", got)
	}
}

func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	scan := worktree.ProcessScan
	worktreetest.StubProcessScanForTest(scan.In, scan.Up, scan.Procs, scan.Agents)
	tb := &hosttest.MainTB{}
	memtest.StubSpawn(tb, true)
	worktree.PeerScan = func(string, []string) ([]worktree.Proc, error) { return nil, nil }
	code := m.Run()
	for _, f := range tb.Cleanups {
		f()
	}
	hosttest.Cleanup()
	cleanup()
	os.Exit(code)
}
