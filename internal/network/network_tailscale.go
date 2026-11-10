package network

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

var tailscalePage = helptext.Page{
	Usage: "claude-code network tailscale [<mode flag>] [flags]",
	About: "Bare prints this box's tailnet status; one mode flag (--machines, --connect,\n" +
		"--key, --exit-node on|off|approve, --subnet <cidr>[,…]|approve|off,\n" +
		"--route <peer>|off) picks another action.",
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

// tsFlags are the values of `network tailscale`'s flags.
type tsFlags struct {
	machines, connect, mint bool
	exitNode, subnet, route string
	format                  *string
	o                       tsOpts
}

func tailscaleFlags() (*flag.FlagSet, *tsFlags) {
	fs := helptext.NewFlagSet("network tailscale", tailscalePage)
	t := &tsFlags{}
	fs.BoolVar(&t.machines, "machines", false, "list tailnet machines")
	fs.BoolVar(&t.connect, "connect", false, "join the tailnet")
	fs.BoolVar(&t.mint, "key", false, "mint a Tailscale auth key")
	fs.StringVar(&t.exitNode, "exit-node", "", "on|off|approve: advertise, stop or approve this box as an exit node")
	fs.StringVar(&t.subnet, "subnet", "", "<cidr>[,…]|approve|off: advertise, approve or withdraw LAN routes")
	fs.StringVar(&t.route, "route", "", "<peer>|off: send this box's traffic through a peer exit node")
	t.format = helptext.FormatFlag(fs)
	o := &t.o
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
	return fs, t
}

// tsMode returns the one mode flag given ("" = status), or an error when several are.
func tsMode(fs *flag.FlagSet, t *tsFlags) (string, error) {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var modes []string
	given := map[string]bool{"machines": t.machines, "connect": t.connect, "key": t.mint,
		"exit-node": set["exit-node"], "subnet": set["subnet"], "route": set["route"]}
	for name, on := range given {
		if on {
			modes = append(modes, "--"+name)
		}
	}
	slices.Sort(modes)
	if len(modes) > 1 {
		return "", fmt.Errorf("%s are mutually exclusive; pick one mode", strings.Join(modes, " and "))
	}
	if len(modes) == 0 {
		return "", nil
	}
	return strings.TrimPrefix(modes[0], "--"), nil
}

// tsWrongFlag reports the first flag that does not belong to mode.
func tsWrongFlag(fs *flag.FlagSet, mode string) error {
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
	return wrong
}

// cmdNetworkTailscale: one flag set; a mode flag picks the action and bare means status.
func cmdNetworkTailscale(args []string) int {
	fs, t := tailscaleFlags()
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return helptext.FailUsage("network tailscale: unexpected argument %q", fs.Arg(0))
	}
	mode, err := tsMode(fs, t)
	if err == nil {
		err = tsWrongFlag(fs, mode)
	}
	if err != nil {
		return helptext.FailUsage("network tailscale: %v", err)
	}
	return tailscaleMode(context.Background(), mode, t)
}

func tailscaleMode(ctx context.Context, mode string, t *tsFlags) int {
	switch mode {
	case "machines":
		return tailscaleMachines(ctx, *t.format)
	case "connect":
		return tailscaleConnect(ctx, t.o)
	case "key":
		return tailscaleKey(ctx, t.o)
	case "exit-node":
		return tailscaleExitNodeChecked(ctx, t.exitNode)
	case "subnet":
		return tailscaleSubnet(ctx, t.subnet)
	case "route":
		return tailscaleRouteChecked(ctx, t.route)
	}
	return tailscaleStatus(ctx, *t.format)
}

func tailscaleExitNodeChecked(ctx context.Context, action string) int {
	if !slices.Contains([]string{"on", "off", "approve"}, action) {
		return helptext.FailUsage("network tailscale: --exit-node must be on, off or approve, got %q", action)
	}
	return tailscaleExitNode(ctx, action)
}

func tailscaleRouteChecked(ctx context.Context, peer string) int {
	if peer == "" || strings.HasPrefix(peer, "-") {
		return helptext.FailUsage("network tailscale: --route requires a peer name or 'off'")
	}
	return tailscaleRoute(ctx, peer)
}

// tailscaleRoute sends this box's traffic through peer as its exit node; "off" clears it.
func tailscaleRoute(ctx context.Context, peer string) int {
	if peer == "off" {
		if err := tailscale.UseExitNode(ctx, ""); err != nil {
			return helptext.Fail("network tailscale route: %v", err)
		}
		audit.Log(audit.TailscaleExitNode, "self", "clear")
		fmt.Println("tailscale: exit node cleared")
		return 0
	}
	if err := tailscale.UseExitNode(ctx, peer); err != nil {
		return helptext.Fail("network tailscale route: %v", err)
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

func tailscaleStatus(ctx context.Context, format string) int {
	if err := helptext.CheckFormat(format); err != nil {
		return helptext.BadFormat("", err)
	}
	st, err := tailscale.Status(ctx)
	if err != nil && !errors.Is(err, tailscale.ErrNotRunning) {
		return helptext.Fail("network tailscale status: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, st); err != nil {
			return helptext.Fail("network tailscale status: %v", err)
		}
		return 0
	}
	fmt.Printf("backend   %s\n", st.BackendState)
	fmt.Printf("node      %s\n", st.DNSName)
	fmt.Printf("ips       %s\n", strings.Join(st.IPs, ", "))
	fmt.Printf("exit-node option=%v approved=%v\n", st.ExitNodeOption, st.ExitNodeApproved)
	if len(st.SubnetRoutes) > 0 {
		fmt.Printf("subnets   approved=%s\n", strings.Join(st.SubnetRoutes, ", "))
	}
	for _, p := range st.Peers {
		fmt.Printf("peer      %-40s %-8s online=%v exit-node=%v\n", p.DNSName, p.OS, p.Online, p.ExitNode)
	}
	return 0
}

func tailscaleMachines(ctx context.Context, format string) int {
	if err := helptext.CheckFormat(format); err != nil {
		return helptext.BadFormat("", err)
	}
	machines, err := tailscale.MachinesCached(ctx)
	if err != nil {
		return helptext.Fail("network tailscale machines: %v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, machines); err != nil {
			return helptext.Fail("network tailscale machines: %v", err)
		}
		return 0
	}
	for _, m := range machines {
		fmt.Printf("%-40s %-10s online=%-5v source=%s\n", m.Name, m.OS, m.Online, m.Source)
	}
	return 0
}

func tailscaleConnect(ctx context.Context, o tsOpts) int {
	if o.authKey != "" && o.authKey != "-" {
		fmt.Fprintln(os.Stderr, "claude-code: network tailscale connect: a literal --auth-key is visible in shell "+
			"history and `ps`; prefer --auth-key - (stdin) or --key-env")
	}
	key, err := tailscale.ResolveAuthKey(o.authKey, o.keyEnv, os.Stdin)
	if err != nil {
		return helptext.Fail("network tailscale connect: reading --auth-key from stdin: %v", err)
	}
	if key == "" {
		return tailscaleConnectKeyless(ctx, o.advertise)
	}
	if err := tailscale.ConnectAudited(ctx, key, o.advertise); err != nil {
		return helptext.Fail("network tailscale connect: %v", err)
	}
	fmt.Println("tailscale: connected")
	return 0
}

// tailscaleConnectKeyless succeeds only when the node is already connected; advertise then applies via `tailscale set`.
func tailscaleConnectKeyless(ctx context.Context, advertise bool) int {
	st, ok := tailscale.Connected(ctx)
	if !ok {
		return helptext.FailUsage("network tailscale connect: not connected; " +
			"pass --auth-key or run `network tailscale --key --save`")
	}
	fmt.Printf("tailscale: already connected (%s)\n", st.DNSName)
	if !advertise {
		return 0
	}
	if err := tailscale.AdvertiseConnected(ctx, st.DNSName); err != nil {
		return helptext.Fail("network tailscale connect: --advertise-exit: %v", err)
	}
	fmt.Println("tailscale: advertising as an exit node")
	return 0
}

func tailscaleKey(ctx context.Context, o tsOpts) int {
	key, err := tailscale.MintKeyAudited(ctx, o.expiry, tailscale.KeyOpts{
		Ephemeral: o.ephemeral, Preauthorized: o.preauth, Reusable: o.reusable,
		Tags: textutil.SplitCSV(o.tags), Description: o.desc,
	})
	if err != nil {
		return helptext.Fail("network tailscale key: %v", err)
	}
	// the only thing on stdout, so it can be piped; a failed write means the key was not delivered
	if _, err := fmt.Fprintln(os.Stdout, key.Key); err != nil {
		return helptext.Fail("network tailscale key: writing key to stdout: %v", err)
	}
	if o.save {
		if err := tailscale.SaveAuthKey(key.Key); err != nil {
			return helptext.Fail("network tailscale key: --save: %v", err)
		}
		fmt.Fprintf(os.Stderr, "saved TAILSCALE_AUTHKEY to the vault (%s)\n", vault.Redact(key.Key))
	}
	return 0
}

func tailscaleExitNode(ctx context.Context, action string) int {
	switch action {
	case "on":
		if err := tailscale.AdvertiseExitNode(ctx, true); err != nil {
			return helptext.Fail("network tailscale exit-node: %v", err)
		}
		audit.Log(audit.TailscaleExitNode, "self", "enable")
		fmt.Println("tailscale: advertising as an exit node")
	case "off":
		if err := tailscale.AdvertiseExitNode(ctx, false); err != nil {
			return helptext.Fail("network tailscale exit-node: %v", err)
		}
		audit.Log(audit.TailscaleExitNode, "self", "disable")
		fmt.Println("tailscale: no longer advertising as an exit node")
	case "approve":
		return tailscaleApproveExitNode(ctx)
	}
	return 0
}

func tailscaleApproveExitNode(ctx context.Context) int {
	dns, err := tailscale.ApproveSelfExitNode(ctx)
	if err != nil {
		return failSelfDevice("exit-node", err)
	}
	audit.Log(audit.TailscaleExitNode, dns, "approve")
	fmt.Println("tailscale: exit-node routes approved")
	return 0
}

// failSelfDevice reports an admin-API failure; a missing credential prints the console hint and exits 2.
func failSelfDevice(mode string, err error) int {
	if errors.Is(err, tailscale.ErrNoCredential) {
		fmt.Println("tailscale: no admin credential; approve at https://login.tailscale.com/admin/machines")
		return 2
	}
	return helptext.Fail("network tailscale %s: %v", mode, err)
}

// tailscaleSubnet dispatches --subnet: "off" withdraws, "approve" approves, anything else is a CIDR list.
func tailscaleSubnet(ctx context.Context, value string) int {
	switch value {
	case "off":
		return tailscaleSubnetWithdraw(ctx)
	case "approve":
		return tailscaleSubnetApprove(ctx)
	}
	return tailscaleSubnetAdvertise(ctx, value)
}

func tailscaleSubnetWithdraw(ctx context.Context) int {
	if err := tailscale.AdvertiseRoutes(ctx, nil); err != nil {
		return helptext.Fail("network tailscale subnet: %v", err)
	}
	audit.Log(audit.TailscaleSubnet, "self", "withdraw")
	fmt.Println("tailscale: subnet routes withdrawn")
	return 0
}

// tailscaleSubnetApprove enables every subnet route this box advertises, keeping routes already enabled.
func tailscaleSubnetApprove(ctx context.Context) int {
	dns, want, enabled, err := tailscale.ApproveSelfSubnets(ctx)
	if err != nil {
		return failSelfDevice("subnet", err)
	}
	audit.Log(audit.TailscaleSubnet, dns, "approve:"+strings.Join(want, ","))
	fmt.Printf("tailscale: subnet routes approved: %s\n", strings.Join(enabled, ", "))
	return 0
}

func tailscaleSubnetAdvertise(ctx context.Context, value string) int {
	routes, ok := tailscale.ParseSubnets(value)
	if !ok {
		return helptext.FailUsage("network tailscale: --subnet must be a CIDR list, approve or off, got %q", value)
	}
	if err := tailscale.AdvertiseRoutes(ctx, routes); err != nil {
		return helptext.Fail("network tailscale subnet: %v", err)
	}
	audit.Log(audit.TailscaleSubnet, "self", "advertise:"+value)
	fmt.Printf("tailscale: advertising %s; approve with --subnet approve\n", strings.Join(routes, ", "))
	return 0
}
