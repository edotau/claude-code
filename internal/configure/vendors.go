// vendors.go — per-vendor sign-in and verify: agy (gemini), gh, agent Set up verify, tailscale admin credential.
package configure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// AgyCommand builds the agy process a sign-in launches; tests replace it with a fake binary.
var AgyCommand = func() *exec.Cmd {
	bin, err := paths.AgentBin("agy")
	if err != nil {
		return &exec.Cmd{Err: err}
	}
	return exec.Command(bin)
}

// AgySignInPoll is how often runAgy checks for agy's own login marker once it's running.
var AgySignInPoll = 500 * time.Millisecond

// AgySignInTimeout bounds how long AgySignIn waits for a sign-in before giving up and stopping agy.
var AgySignInTimeout = 10 * time.Minute

// AgyStopGrace is how long SIGINT gets to land before AgySignIn escalates to SIGTERM, then to SIGKILL.
var AgyStopGrace = 3 * time.Second

// AgySignIn starts agy on the real terminal, polls for its login marker every AgySignInPoll, and stops it
// (SIGINT, SIGTERM, SIGKILL, each after AgyStopGrace) once the marker appears, on AgySignInTimeout, or when
// agy exits on its own. The caller never has to return to a menu with agy's own TUI still on screen.
func AgySignIn() (ok bool, msg string) {
	defer proc.Preserve()()
	home, _ := os.UserHomeDir()
	marker := gemini.AgyLoginMarker(home)
	before, _ := os.ReadFile(marker)
	beforeInfo, _ := os.Stat(marker)
	cmd := AgyCommand()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return false, fmt.Sprintf("agy: %v", err)
	}
	done := make(chan struct{}) // closed on exit; the login marker, not agy's exit status, decides sign-in
	go func() { _ = cmd.Wait(); close(done) }()
	deadline := time.Now().Add(AgySignInTimeout)
	ticker := time.NewTicker(AgySignInPoll)
	defer ticker.Stop()
	for {
		if agySignedInSince(marker, before, beforeInfo) {
			stopAgy(cmd, done)
			return true, fmt.Sprintf("%-10s signed in", "agy")
		}
		select {
		case <-done:
			if agySignedInSince(marker, before, beforeInfo) { // agy may have written the marker right before exiting
				return true, fmt.Sprintf("%-10s signed in", "agy")
			}
			return false, "agy: not signed in"
		case now := <-ticker.C:
			if now.After(deadline) {
				stopAgy(cmd, done)
				return false, "agy: not signed in (timed out)"
			}
		}
	}
}

func agySignedInSince(marker string, before []byte, beforeInfo os.FileInfo) bool {
	after, err := os.ReadFile(marker)
	if err != nil || len(after) == 0 {
		return false
	}
	if !bytes.Equal(after, before) {
		return true
	}
	afterInfo, err := os.Stat(marker)
	return err == nil && (beforeInfo == nil || !afterInfo.ModTime().Equal(beforeInfo.ModTime()))
}

// stopAgy stops a still-running agy and its descendants: SIGINT, then SIGTERM, then SIGKILL, each after AgyStopGrace.
func stopAgy(cmd *exec.Cmd, done <-chan struct{}) {
	desc := proc.Descendants(cmd.Process.Pid)
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		_ = cmd.Process.Signal(sig)
		signalAliveDescendants(desc, sig)
		if waitAgy(done, desc) {
			return
		}
	}
	_ = cmd.Process.Kill()
	signalAliveDescendants(desc, syscall.SIGKILL)
	<-done
}

func waitAgy(done <-chan struct{}, pids []int) bool {
	deadline := time.NewTimer(AgyStopGrace)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			if !slices.ContainsFunc(pids, proc.Alive) {
				return true
			}
		default:
		}
		select {
		case <-deadline.C:
			return false
		case <-ticker.C:
		}
	}
}

func signalDescendants(pids []int, sig syscall.Signal) {
	self := os.Getpid()
	for _, pid := range pids {
		if pid <= 1 || pid == self {
			continue
		}
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(sig)
		}
	}
}

func signalAliveDescendants(pids []int, sig syscall.Signal) {
	var alive []int
	for _, pid := range pids {
		if proc.Alive(pid) {
			alive = append(alive, pid)
		}
	}
	signalDescendants(alive, sig)
}

// ghAccountRE pulls the account name out of `gh auth status`'s "Logged in to github.com account <name>" line.
var ghAccountRE = regexp.MustCompile(`account\s+(\S+)`)

// ghStatus is what `gh auth status` reports: whether the binary exists, whether it is logged in, and the
// account name — never a token (gh itself only ever prints a masked one).
type ghStatus struct {
	Found    bool
	LoggedIn bool
	Account  string
}

// GHAuthCheck runs `gh auth status` (8s cap); its exit code is the verdict, its stdout+stderr the account line.
func GHAuthCheck() ghStatus {
	bin, err := exec.LookPath("gh")
	if err != nil {
		return ghStatus{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "auth", "status").CombinedOutput()
	st := ghStatus{Found: true, LoggedIn: err == nil}
	if m := ghAccountRE.FindStringSubmatch(string(out)); len(m) > 1 {
		st.Account = m[1]
	}
	return st
}

// WriteGithubCopilotModels validates and merges picks and fallback into vault overlay/providers.
func WriteGithubCopilotModels(picks map[string]string, fallback string) error {
	for _, alias := range []string{models.Opus, models.Sonnet, models.Haiku} {
		if strings.TrimSpace(picks[alias]) == "" {
			return fmt.Errorf("github-copilot models: %s model is empty", alias)
		}
	}
	if fallback == "auto" {
		return errors.New(`github-copilot opencode_fallback: "auto" is not a valid fallback (opencode's own catalog has no alias by that name)`)
	}
	fields := map[string]any{"models": picks}
	if fallback != "" {
		fields["opencode_fallback"] = fallback
	}
	reg, err := providers.Load()
	if err != nil {
		return err
	}
	p := reg.ProfileBy("github", providers.AuthCLI)
	if p == nil {
		return errors.New("github models: no github profile in the registry")
	}
	return providers.SetProviderFields(p.Name, fields)
}

// setupVerifyTimeout caps the one small real request the verify step sends.
const setupVerifyTimeout = 2 * time.Minute

// QuotaFailure reports a verify error that means exhausted credits or a rate limit, not bad credentials.
func QuotaFailure(err error) bool {
	s := err.Error()
	return strings.Contains(s, "quota") || strings.Contains(s, "rate_limit")
}

// VerifyAgent asks the agent for "pong" through `claude-code agents ask`, the same request preflight sends.
func VerifyAgent(ctx context.Context, agent string) error {
	bin, err := paths.SelfBinary()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, setupVerifyTimeout)
	defer cancel()
	pin := preflight.Pin{Agent: agent, Status: preflight.StatusOK, Served: "n/a"}
	for _, p := range preflight.LivePongs(ctx, bin, []preflight.Pin{pin}) {
		if !p.OK {
			return errors.New(p.Err)
		}
	}
	return nil
}

// HasAdminCredential reports, by name only, whether a tailscale admin API credential is set.
func HasAdminCredential() bool {
	for _, n := range []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET", "TAILSCALE_API_KEY"} {
		if src, _ := vault.HasSecretErr(n); src != "" { // presence only: no decrypt per home render
			return true
		}
	}
	return false
}
