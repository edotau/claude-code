package launch

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/signin"
)

// Test seams: the sign-in factory, the TTY gate, and the prompt's input and output.
var (
	signinFor           = signin.For
	canPrompt           = func() bool { return ansi.IsTerminal(os.Stdin) && ansi.IsTerminal(os.Stderr) }
	loginIn   io.Reader = os.Stdin
	loginOut  io.Writer = os.Stderr
	// credCheckTimeout bounds the pre-launch sign-in check; a var so tests stay fast.
	credCheckTimeout = 12 * time.Second
)

// offerLogin offers a sign-in for each provider whose check says it needs one; failures warn.
func offerLogin(ctx context.Context, o Options, args []string, ps []*providers.Provider, plan *Plan) {
	if o.PrintEnv || headless(args) || !canPrompt() {
		return
	}
	seen := map[string]bool{}
	var todo []*providers.Provider
	for _, p := range ps {
		if p == nil || seen[p.Name] {
			continue
		}
		if p.Auth.Type == providers.AuthPassthrough && p.Auth.Login == "" { // Claude Code shows its own login screen
			continue
		}
		seen[p.Name] = true
		todo = append(todo, p)
	}
	checks := make([]checkResult, len(todo))
	var wg sync.WaitGroup
	for i, p := range todo {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := signinFor(p)
			v, d, ok := checkSignin(ctx, s)
			checks[i] = checkResult{s, v, d, ok}
		}()
	}
	wg.Wait()
	in := bufio.NewReader(loginIn) // one reader: a per-provider reader would swallow the next prompt's answer
	for i, p := range todo {
		if err := offerLoginOne(ctx, p, in, checks[i]); err != nil {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: %v", p.Name, err))
		}
	}
}

// checkResult is one provider's concurrent pre-launch check.
type checkResult struct {
	s      signin.Signin
	v      providers.AuthVerdict
	detail string
	ok     bool
}

// offerLoginOne prompts and signs in when p's check says it must, then rechecks once.
func offerLoginOne(ctx context.Context, p *providers.Provider, in *bufio.Reader, c checkResult) error {
	s, v, ok := c.s, c.v, c.ok
	if !ok {
		// A timeout is transient and never means a login is needed (A1 `transient`: no auth mark recorded).
		return fmt.Errorf("sign-in check timed out after %s; launching anyway", credCheckTimeout)
	}
	if !v.Condemns() {
		return nil
	}
	fmt.Fprintf(loginOut, "%s: login required. Log in now? [Y/n] ", p.Name)
	line, _ := in.ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "" && !strings.HasPrefix(a, "y") {
		if p.Auth.AuthType == "" && p.CLIProfile != "" {
			return fmt.Errorf("login skipped; requests will fail until `databricks auth login -p %s` (%s)", p.CLIProfile, v)
		}
		return fmt.Errorf("login skipped; requests will fail until `claude-code configure provider --all %s` (%s)", p.Name, v)
	}
	if err := s.Login(ctx, loginIn, loginOut); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	var detail string
	v, detail, ok = checkSignin(ctx, s)
	if !ok {
		return fmt.Errorf("sign-in check timed out after %s; launching anyway", credCheckTimeout)
	}
	if v.Condemns() {
		return fmt.Errorf("still %s after login: %s", v, detail)
	}
	return nil
}

// checkSignin runs the check, giving up after credCheckTimeout (ok=false); an abandoned one dies with the exec.
func checkSignin(ctx context.Context, s signin.Signin) (v providers.AuthVerdict, detail string, ok bool) {
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
