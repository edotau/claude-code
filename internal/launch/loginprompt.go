package launch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/providers"
)

// Test seams: the browser login, the TTY gate, and the prompt's input and output.
var (
	loginRow            = databricks.LoginRow
	canPrompt           = func() bool { return ansi.IsTerminal(os.Stdin) && ansi.IsTerminal(os.Stderr) }
	loginIn   io.Reader = os.Stdin
	loginOut  io.Writer = os.Stderr
)

// offerLogin offers `databricks login` for each interactive u2m provider whose credential needs one; failures warn.
func offerLogin(ctx context.Context, o Options, args []string, ps []*providers.Provider, plan *Plan) {
	if o.PrintEnv || headless(args) || !canPrompt() {
		return
	}
	seen := map[string]bool{}
	in := bufio.NewReader(loginIn) // one reader: a per-provider reader would swallow the next prompt's answer
	for _, p := range ps {
		if p == nil || p.Auth.AuthType != providers.AuthU2M || seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		if err := offerLoginOne(ctx, p, in); err != nil {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: %v", p.Name, err))
		}
	}
}

// offerLoginOne prompts and logs in when p's credential needs it, then retries the credential once.
func offerLoginOne(ctx context.Context, p *providers.Provider, in *bufio.Reader) error {
	_, err := providers.Credential(ctx, p)
	if !errors.Is(err, providers.ErrNeedsLogin) {
		return nil
	}
	fmt.Fprintf(loginOut, "%s: Databricks login required. Log in now? [Y/n] ", p.Name)
	line, _ := in.ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "" && !strings.HasPrefix(a, "y") {
		return fmt.Errorf("login skipped; requests will fail until `claude-code databricks login %s`", p.Name)
	}
	if err := loginRow(ctx, p.Name, loginOut); err != nil {
		return fmt.Errorf("databricks login: %w", err)
	}
	if _, err := providers.Credential(ctx, p); err != nil {
		return fmt.Errorf("still no credential after login: %w", err)
	}
	return nil
}
