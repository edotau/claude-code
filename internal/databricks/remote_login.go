package databricks

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Seams: tests swap this machine's opener and the two ssh calls of a --remote login.
var (
	openLocal   = proc.OpenURL
	forwardPort = remote.ReverseForward
	openOnHost  = remote.OpenURLOn
)

// remoteRange is the host ports a --remote login tries: the exact --port N, else the local port up to the span's end.
func (h headless) remoteRange(local int) (first, last int) {
	if h.port > 0 {
		return h.port, h.port
	}
	return local, oauth.DefaultPort + oauth.PortSpan
}

// remoteOptions is the oauth.LoginOptions of a --remote login: the browser opens on h.via and its callback is forwarded here.
func (h headless) remoteOptions() oauth.LoginOptions {
	o := oauth.LoginOptions{Out: os.Stderr}
	if h.port > 0 {
		o.Port, o.ExactPort = h.port, true
	}
	if h.paste {
		o.Paste = loginIn
	}
	up := false
	o.Open = func(authURL string) error {
		if !up {
			return fmt.Errorf("no forward to %s, so no browser was opened there", h.via)
		}
		return openOnHost(context.Background(), h.via, authURL)
	}
	o.Forward = func(ctx context.Context, local int) (int, func(), error) {
		first, last := h.remoteRange(local)
		port, stop, err := forwardPort(ctx, h.via, first, last, local)
		if err == nil {
			up = true
			return port, stop, nil
		}
		if !h.paste {
			return 0, nil, remoteForwardError(h.via, err)
		}
		fmt.Fprintf(os.Stderr, "%v; continuing with --paste, no forward\n", remoteForwardError(h.via, err))
		return local, func() {}, nil
	}
	return o
}

// remoteForwardError is the one-line failure of a reverse forward: a taken port, or ssh itself, with the --paste way out.
func remoteForwardError(host string, err error) error {
	if taken, ok := errors.AsType[*remote.PortTakenError](err); ok {
		return fmt.Errorf("%w; pass --port N for another port, or --paste to sign in without the forward", taken)
	}
	return fmt.Errorf("cannot reach %s over ssh (%w); pass --paste to sign in without the forward", host, err)
}

// describeRemote is the dry-run line of a --remote login: where the browser opens and where the callback travels.
func (h headless) describeRemote(profile string) string {
	port := fmt.Sprintf("%s:%d (next free port up to %d if taken)", h.via, oauth.DefaultPort, oauth.DefaultPort+oauth.PortSpan)
	if h.port > 0 {
		port = fmt.Sprintf("%s:%d (exact; error if taken)", h.via, h.port)
	}
	return fmt.Sprintf("would sign the user in for %s: browser on %s, callback forwarded from %s to this machine, "+
		"token sealed in this machine's vault", profile, h.via, port)
}

// checkRemote refuses --no-browser under --remote: the browser always opens on the host and this machine opens none.
func checkRemote(h headless) (code int, bad bool) {
	if h.via != "" && h.noBrowser {
		fail("login: --no-browser contradicts --remote: the host's browser is the one that opens; drop it, " +
			"or pass --paste to also accept the redirected URL")
		return 2, true
	}
	return 0, false
}

var openURLPage = helptext.Page{
	Usage: "claude-code databricks open-url <https-url>",
	About: "Open a sign-in URL in this machine's browser; `login` under --remote runs it on the host over ssh.",
}

// openURLVerb is `databricks open-url`: it opens one https URL with the desktop opener and nothing else.
func openURLVerb(args []string) int {
	fs := helptext.NewFlagSet("databricks open-url", openURLPage)
	names, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(names) != 1 {
		return helptext.FailUsage("databricks open-url: want exactly one https URL")
	}
	if u, err := url.Parse(names[0]); err != nil || u.Scheme != "https" || u.Host == "" || strings.HasPrefix(names[0], "-") {
		return helptext.FailUsage("databricks open-url: not an https URL")
	}
	if err := openLocal(names[0]); err != nil {
		return fail("open-url: %v", err)
	}
	return 0
}
