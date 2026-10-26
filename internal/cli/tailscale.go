package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tailscale"
)

var tailscalePage = helptext.Page{
	Usage: "claude-code network tailscale [<mode flag>] [flags]",
	About: "Bare prints this box's tailnet status; one mode flag (--machines, --connect,\n" +
		"--key, --exit-node on|off|approve, --route <peer>|off) picks another action.",
	Hint: "--connect takes --auth-key k|-, --key-env NAME and --advertise-exit.\n" +
		"--key takes --expiry, --tags, --save, --reusable, --ephemeral, --preauthorized\n" +
		"and --description. Status and --machines take --format text|json.",
}

// tsFlagMode maps each mode-specific flag to the mode flag that owns it ("" = status or machines).
var tsFlagMode = map[string]string{
	"auth-key": "connect", "key-env": "connect", "advertise-exit": "connect",
	"expiry": "key", "tags": "key", "save": "key", "reusable": "key", "ephemeral": "key",
	"preauthorized": "key", "description": "key",
}

// cmdNetworkTailscale: one flag set; a mode flag picks the action and bare means status.
func cmdNetworkTailscale(args []string) int {
	fs := helptext.NewFlagSet("network tailscale", tailscalePage)
	machines := fs.Bool("machines", false, "list tailnet machines")
	connect := fs.Bool("connect", false, "join the tailnet")
	mint := fs.Bool("key", false, "mint a Tailscale auth key")
	exitNode := fs.String("exit-node", "", "on|off|approve: advertise, stop or approve this box as an exit node")
	route := fs.String("route", "", "<peer>|off: send this box's traffic through a peer exit node")
	format := outfmt.Flag(fs)
	var o tsOpts
	fs.StringVar(&o.authKey, "auth-key", "", "an auth key, or - to read it from stdin (tskey-auth-…)")
	fs.StringVar(&o.keyEnv, "key-env", "", "read the auth key from this env var instead")
	fs.BoolVar(&o.advertise, "advertise-exit", false, "also advertise this box as an exit node")
	fs.BoolVar(&o.ephemeral, "ephemeral", false, "the key's device is removed on logout/offline")
	fs.BoolVar(&o.preauth, "preauthorized", false, "new devices skip admin approval")
	fs.BoolVar(&o.reusable, "reusable", false, "the key can register more than one device")
	fs.StringVar(&o.tags, "tags", "", "comma-separated ACL tags (tag:a,tag:b)")
	fs.StringVar(&o.expiry, "expiry", "90d", "key lifetime: a Go duration, or Nd for N days")
	fs.StringVar(&o.desc, "description", "", "a note shown in the admin console")
	fs.BoolVar(&o.save, "save", false, "also seal the key as TAILSCALE_AUTHKEY in the vault")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return failUsage("network tailscale: unexpected argument %q", fs.Arg(0))
	}
	exitNodeSet, routeSet := false, false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "exit-node" {
			exitNodeSet = true
		}
		if f.Name == "route" {
			routeSet = true
		}
	})
	var modes []string
	for name, on := range map[string]bool{"machines": *machines, "connect": *connect, "key": *mint, "exit-node": exitNodeSet, "route": routeSet} {
		if on {
			modes = append(modes, "--"+name)
		}
	}
	slices.Sort(modes)
	if len(modes) > 1 {
		return failUsage("network tailscale: %s are mutually exclusive; pick one mode", strings.Join(modes, " and "))
	}
	mode := ""
	if len(modes) == 1 {
		mode = strings.TrimPrefix(modes[0], "--")
	}
	var wrong error
	fs.Visit(func(f *flag.Flag) {
		owner, ok := tsFlagMode[f.Name]
		if ok && owner != mode && wrong == nil {
			wrong = fmt.Errorf("--%s belongs to --%s", f.Name, owner)
		}
		if f.Name == "format" && mode != "" && mode != "machines" && wrong == nil {
			wrong = errors.New("--format applies to status and --machines only")
		}
	})
	if wrong != nil {
		return failUsage("network tailscale: %v", wrong)
	}
	ctx := context.Background()
	switch mode {
	case "machines":
		return tailscaleMachines(ctx, *format)
	case "connect":
		return tailscaleConnect(ctx, o)
	case "key":
		return tailscaleKey(ctx, o)
	case "exit-node":
		if !slices.Contains([]string{"on", "off", "approve"}, *exitNode) {
			return failUsage("network tailscale: --exit-node must be on, off or approve, got %q", *exitNode)
		}
		return tailscaleExitNode(ctx, *exitNode)
	case "route":
		if *route == "" || strings.HasPrefix(*route, "-") {
			return failUsage("network tailscale: --route requires a peer name or 'off'")
		}
		return tailscaleRoute(ctx, *route)
	}
	return tailscaleStatus(ctx, *format)
}

// tailscaleRoute sends this box's traffic through peer as its exit node; "off" clears it.
func tailscaleRoute(ctx context.Context, peer string) int {
	if peer == "off" {
		if err := tailscale.UseExitNode(ctx, ""); err != nil {
			return fail("network tailscale route: %v", err)
		}
		audit.Log(audit.TailscaleExitNode, "self", "clear")
		fmt.Println("tailscale: exit node cleared")
		return 0
	}
	if err := tailscale.UseExitNode(ctx, peer); err != nil {
		return fail("network tailscale route: %v", err)
	}
	audit.Log(audit.TailscaleExitNode, peer, "use")
	fmt.Println("tailscale: routing via " + peer + " (LAN stays local)")
	return 0
}

// tsOpts carries the connect and key mode flags.
type tsOpts struct {
	authKey, keyEnv, tags, expiry, desc string
	advertise, ephemeral, preauth       bool
	reusable, save                      bool
}

func failUsage(format string, a ...any) int {
	fail(format, a...)
	return 2
}

func tailscaleStatus(ctx context.Context, format string) int {
	if err := outfmt.Check(format); err != nil {
		return fail("%v", err)
	}
	st, err := tailscale.Status(ctx)
	if err != nil && !errors.Is(err, tailscale.ErrNotRunning) {
		return fail("network tailscale status: %v", err)
	}
	if format == outfmt.JSON {
		if err := outfmt.Write(os.Stdout, st); err != nil {
			return fail("network tailscale status: %v", err)
		}
		return 0
	}
	fmt.Printf("backend   %s\n", st.BackendState)
	fmt.Printf("node      %s\n", st.DNSName)
	fmt.Printf("ips       %s\n", strings.Join(st.IPs, ", "))
	fmt.Printf("exit-node option=%v approved=%v\n", st.ExitNodeOption, st.ExitNodeApproved)
	for _, p := range st.Peers {
		fmt.Printf("peer      %-40s %-8s online=%v exit-node=%v\n", p.DNSName, p.OS, p.Online, p.ExitNode)
	}
	return 0
}

func tailscaleMachines(ctx context.Context, format string) int {
	if err := outfmt.Check(format); err != nil {
		return fail("%v", err)
	}
	machines, err := tailscale.Machines(ctx)
	if err != nil {
		return fail("network tailscale machines: %v", err)
	}
	names := make([]string, 0, len(machines))
	for _, m := range machines {
		names = append(names, m.Name)
	}
	writeDispatchCache(names) // `dispatch --host` completion reads this cache; never calls the network itself
	if format == outfmt.JSON {
		if err := outfmt.Write(os.Stdout, machines); err != nil {
			return fail("network tailscale machines: %v", err)
		}
		return 0
	}
	for _, m := range machines {
		fmt.Printf("%-40s %-10s online=%-5v source=%s\n", m.Name, m.OS, m.Online, m.Source)
	}
	return 0
}

func tailscaleConnect(ctx context.Context, o tsOpts) int {
	key := o.authKey
	if key == "-" {
		in, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fail("network tailscale connect: reading --auth-key from stdin: %v", err)
		}
		key = strings.TrimSpace(string(in))
	} else if key != "" {
		fmt.Fprintln(os.Stderr, "tailscale connect: a literal --auth-key is visible in shell "+
			"history and `ps`; prefer --auth-key - (stdin) or --key-env")
	}
	if key == "" && o.keyEnv != "" {
		key = strings.TrimSpace(os.Getenv(o.keyEnv))
	}
	if key == "" {
		key, _, _ = secrets.Get("TAILSCALE_AUTHKEY")
	}
	if key == "" {
		if st, err := tailscale.Status(ctx); err == nil && st.BackendState == "Running" {
			fmt.Printf("tailscale: already connected (%s)\n", st.DNSName)
			return 0
		}
		fmt.Fprintln(os.Stderr, "tailscale: not connected; pass --auth-key or run `network tailscale --key --save`")
		return 2
	}
	if err := tailscale.Connect(ctx, key, o.advertise); err != nil {
		return fail("network tailscale connect: %v", err)
	}
	st, err := tailscale.Status(ctx)
	if err != nil {
		audit.Log(audit.TailscaleConnect, "", "connect (status unavailable: "+err.Error()+")")
	} else {
		audit.Log(audit.TailscaleConnect, st.DNSName, "connect")
	}
	fmt.Println("tailscale: connected")
	return 0
}

func tailscaleKey(ctx context.Context, o tsOpts) int {
	exp, err := parseKeyExpiry(o.expiry)
	if err != nil {
		return fail("network tailscale key: --expiry: %v", err)
	}
	client, err := tailscale.NewClient()
	if err != nil {
		return fail("network tailscale key: %v", err)
	}
	var tagList []string
	if o.tags != "" {
		tagList = strings.Split(o.tags, ",")
	}
	key, err := client.CreateAuthKey(ctx, tailscale.KeyOpts{
		Ephemeral: o.ephemeral, Preauthorized: o.preauth, Reusable: o.reusable,
		Tags: tagList, Expiry: exp, Description: o.desc,
	})
	if err != nil {
		// The admin API answers 404, not 403, when an OAuth client lacks the auth_keys scope.
		if strings.Contains(err.Error(), "HTTP 404") {
			return fail("network tailscale key: %v\nhint: the OAuth client likely lacks Keys → Auth Keys: write, "+
				"and OAuth-minted keys need --tags tag:<x> (a tag in tagOwners); or use TAILSCALE_API_KEY", err)
		}
		return fail("network tailscale key: %v", err)
	}
	audit.Log(audit.TailscaleKeyMint, "tailnet", "mint:"+strings.Join(tagList, ","))
	fmt.Println(key.Key) // the only thing on stdout, so it can be piped
	if o.save {
		if err := secrets.Store("TAILSCALE_AUTHKEY", key.Key); err != nil {
			fmt.Fprintf(os.Stderr, "tailscale key: --save: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "saved TAILSCALE_AUTHKEY to the vault (%s)\n", secrets.Redact(key.Key))
	}
	return 0
}

// parseKeyExpiry accepts a Go duration or a bare "<N>d" day count.
func parseKeyExpiry(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func tailscaleExitNode(ctx context.Context, action string) int {
	switch action {
	case "on":
		if err := tailscale.AdvertiseExitNode(ctx, true); err != nil {
			return fail("network tailscale exit-node: %v", err)
		}
		audit.Log(audit.TailscaleExitNode, "self", "enable")
		fmt.Println("tailscale: advertising as an exit node")
	case "off":
		if err := tailscale.AdvertiseExitNode(ctx, false); err != nil {
			return fail("network tailscale exit-node: %v", err)
		}
		audit.Log(audit.TailscaleExitNode, "self", "disable")
		fmt.Println("tailscale: no longer advertising as an exit node")
	case "approve":
		return tailscaleApproveExitNode(ctx)
	}
	return 0
}

func tailscaleApproveExitNode(ctx context.Context) int {
	client, err := tailscale.NewClient()
	if err != nil {
		fmt.Println("tailscale: no admin credential; approve at https://login.tailscale.com/admin/machines")
		return 2
	}
	st, err := tailscale.Status(ctx)
	if err != nil { // ErrNotRunning included: st.IPs would be stale and never match a device
		return fail("network tailscale exit-node: %v", err)
	}
	devices, err := client.Devices(ctx)
	if err != nil {
		return fail("network tailscale exit-node: %v", err)
	}
	var id string
	for _, d := range devices {
		if slices.ContainsFunc(d.Addresses, func(a string) bool { return slices.Contains(st.IPs, a) }) {
			id = d.ID
			break
		}
	}
	if id == "" {
		return fail("network tailscale exit-node: could not match this node's address to a device (is it connected?)")
	}
	if err := client.ApproveExitNode(ctx, id); err != nil {
		return fail("network tailscale exit-node: %v", err)
	}
	audit.Log(audit.TailscaleExitNode, st.DNSName, "approve")
	fmt.Println("tailscale: exit-node routes approved")
	return 0
}
