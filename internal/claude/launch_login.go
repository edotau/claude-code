package claude

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/launchplan"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
)

// Test seams: the sign-in factory, the TTY gate, and the prompt's input and output.
var (
	signinFor           = agent.SigninFor
	canPrompt           = func() bool { return launchplan.IsTerminal(os.Stdin) && launchplan.IsTerminal(os.Stderr) }
	loginIn   io.Reader = os.Stdin
	loginOut  io.Writer = os.Stderr
	// credCheckTimeout bounds the pre-launch sign-in check; a var so tests stay fast.
	credCheckTimeout = 12 * time.Second
)

// offerLogin offers a sign-in for each provider whose check says it needs one; failures warn.
func offerLogin(ctx context.Context, o agent.LaunchOptions, args []string, ps []*providers.Provider, plan *agent.Plan) {
	if o.PrintEnv || launchplan.IsHeadless(args) || !canPrompt() {
		return
	}
	todo := loginCandidates(ps)
	checks := runChecks(ctx, todo)
	in := bufio.NewReader(loginIn) // one reader: a per-provider reader would swallow the next prompt's answer
	loggedIn := false
	for i, p := range todo {
		ok, err := offerLoginOne(ctx, p, in, checks[i])
		loggedIn = loggedIn || ok
		if err != nil {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: %v", p.Name, err))
		}
	}
	if loggedIn { // models differ per account: refresh copilot's and opencode's lists once, detached
		if reg, err := providers.Load(); err == nil {
			preflight.DiscoverAfterAuth(reg)
		}
	}
}

// loginCandidates dedups ps by name and drops nil entries and Claude Code's own login screen.
func loginCandidates(ps []*providers.Provider) []*providers.Provider {
	seen := map[string]bool{}
	var todo []*providers.Provider
	for _, p := range ps {
		if p == nil || seen[p.Name] {
			continue
		}
		if p.Auth.Method() == providers.AuthCLI && p.Auth.Login == "" { // Claude Code shows its own login screen
			continue
		}
		seen[p.Name] = true
		todo = append(todo, p)
	}
	return todo
}

// runChecks runs every provider's sign-in check concurrently, results in todo order.
func runChecks(ctx context.Context, todo []*providers.Provider) []checkResult {
	checks := make([]checkResult, len(todo))
	var wg sync.WaitGroup
	for i, p := range todo {
		wg.Go(func() {
			s := signinFor(p)
			v, d, ok := checkSignin(ctx, s)
			checks[i] = checkResult{s, v, d, ok}
		})
	}
	wg.Wait()
	return checks
}

// checkResult is one provider's concurrent pre-launch check.
type checkResult struct {
	s      agent.Signin
	v      providers.AuthVerdict
	detail string
	ok     bool
}

// offerLoginOne prompts and signs in when p's check says it must, then rechecks once.
func offerLoginOne(ctx context.Context, p *providers.Provider, in *bufio.Reader, c checkResult) (loggedIn bool, err error) {
	s, v, ok := c.s, c.v, c.ok
	if !ok {
		// A timeout is transient and never means a login is needed (A1 `transient`: no auth mark recorded).
		return false, fmt.Errorf("sign-in check timed out after %s; launching anyway", credCheckTimeout)
	}
	if !v.Condemns() {
		return false, nil
	}
	if !consentToLogin(p, in) {
		return false, loginSkipped(p, v)
	}
	if err := s.Login(ctx, loginIn, loginOut); err != nil {
		return false, fmt.Errorf("login: %w", err)
	}
	var detail string
	v, detail, ok = checkSignin(ctx, s)
	if !ok {
		return true, fmt.Errorf("sign-in check timed out after %s; launching anyway", credCheckTimeout)
	}
	if v.Condemns() {
		return true, fmt.Errorf("still %s after login: %s", v, detail)
	}
	return true, nil
}

// consentToLogin prompts for a login; EOF or a read error is no consent, and an empty answer means yes.
func consentToLogin(p *providers.Provider, in *bufio.Reader) bool {
	fmt.Fprintf(loginOut, "%s: login required. Log in now? [Y/n] ", p.Name)
	line, rerr := in.ReadString('\n')
	if rerr != nil && (!errors.Is(rerr, io.EOF) || strings.TrimSpace(line) == "") {
		return false // a read error, or EOF with no answer, is no consent
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "" || strings.HasPrefix(a, "y")
}

// loginSkipped is the warning for a declined login, naming the manual command.
func loginSkipped(p *providers.Provider, v providers.AuthVerdict) error {
	if p.Auth.Method() == "" && p.CLIProfile != "" {
		return fmt.Errorf("login skipped; requests will fail until `databricks auth login -p %s` (%s)", p.CLIProfile, v)
	}
	return fmt.Errorf("login skipped; requests will fail until `claude-code configure provider --all %s` (%s)", p.Name, v)
}

// checkSignin runs the check, giving up after credCheckTimeout (ok=false); an abandoned one is cancelled on return.
func checkSignin(ctx context.Context, s agent.Signin) (v providers.AuthVerdict, detail string, ok bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type res struct {
		v      providers.AuthVerdict
		detail string
	}
	done := make(chan res, 1)
	go func() { v, d := s.Check(ctx); done <- res{v, d} }()
	select {
	case r := <-done:
		return r.v, r.detail, true
	case <-time.After(credCheckTimeout):
		return "", "", false
	}
}
