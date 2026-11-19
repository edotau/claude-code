package tailscale

import (
	"cmp"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

// stepUpWait bounds the browser re-authentication.
var stepUpWait = 5 * time.Minute

// Seams: tests stub the terminal check and the re-auth.
var (
	stdioIsTTY = func() bool { return proc.IsTerminal(os.Stdin) && proc.IsTerminal(os.Stdout) }
	reauthRun  = reauth
	stepUpGate = stepUp
)

var tailnetNets = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("fd7a:115c:a1e0::/48")}

// stepUp gates a key mint: an owner terminal, no Tailscale SSH session, then a browser re-auth by the same user.
func stepUp(ctx context.Context, out io.Writer) error {
	if proc.InDispatch(ctx) || !stdioIsTTY() {
		return errors.New("minting a key needs your own terminal (stdin and stdout a TTY, not inside an agent " +
			"dispatch): run `claude-code network tailscale --token` there yourself")
	}
	if inTailscaleSSH(os.Getenv("SSH_CONNECTION")) {
		return errors.New("this shell is a Tailscale SSH session to this machine and re-auth would drop it: " +
			"run it from the machine's own terminal")
	}
	before, err := Status(ctx)
	if err != nil || before.LoginName == "" {
		return fmt.Errorf("cannot read the signed-in tailscale user before re-auth: %w", cmpErr(err))
	}
	fmt.Fprintln(out, "tailscale: re-authenticating rotates this node's key and drops its connections for a few seconds")
	if err := reauthRun(ctx, out); err != nil {
		return fmt.Errorf("re-auth: %w", err)
	}
	after, err := Status(ctx)
	if err != nil || after.LoginName != before.LoginName {
		return errors.New("re-auth signed in a different user (or status is unreadable); no key minted")
	}
	return nil
}

func cmpErr(err error) error {
	if err == nil {
		return errors.New("status names no user")
	}
	return err
}

// inTailscaleSSH is true when SSH_CONNECTION's client address is a tailnet address.
func inTailscaleSSH(conn string) bool {
	f := strings.Fields(conn)
	if len(f) == 0 {
		return false
	}
	ip, err := netip.ParseAddr(f[0])
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, n := range tailnetNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// reauth forces a browser sign-in through the LocalAPI, else `tailscale login`, and waits for Running.
func reauth(ctx context.Context, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, stepUpWait)
	defer cancel()
	err := reauthLocal(ctx, out)
	if !errors.Is(err, errNoLocalAPI) {
		return err
	}
	bin, err := tailscaleBin()
	if err != nil {
		return err
	}
	cmd := execCommandContext(ctx, bin, cliArgs("login")...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, out, os.Stderr
	return execErr(ctx, "login", cmd.Run())
}

func reauthLocal(ctx context.Context, out io.Writer) error {
	bus, err := localAPIStream(ctx, "watch-ipn-bus?mask="+strconv.Itoa(notifyInitialState))
	if err != nil {
		return err
	}
	defer func() { _ = bus.Close() }()
	if _, err := localAPI(ctx, http.MethodPost, "login-interactive", nil); err != nil {
		return err
	}
	return awaitReauth(bus, out)
}

// awaitReauth prints the first fresh sign-in URL, then returns at the next Running; the initial notify is stale.
func awaitReauth(bus io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(bus)
	sc.Buffer(nil, maxNotify)
	for first, shown := true, false; sc.Scan(); first = false {
		var n notify
		if err := json.Unmarshal(sc.Bytes(), &n); err != nil {
			return fmt.Errorf("tailscale localapi watch-ipn-bus: %w", err)
		}
		if _, err := n.outcome(); err != nil {
			return err
		}
		if u, ok := signInURL(n.BrowseToURL); ok && !first && !shown {
			fmt.Fprintf(out, "\nTo re-authenticate, visit:\n\n\t%s\n\n", u)
			shown = true
		}
		if shown && n.State != nil && *n.State == ipnRunning {
			return nil
		}
	}
	return fmt.Errorf("tailscale localapi watch-ipn-bus: %w", cmp.Or(sc.Err(), io.ErrUnexpectedEOF))
}
