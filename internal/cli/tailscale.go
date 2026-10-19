package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/tailscale"
)

const tailscaleUsage = "usage: claude-code tailscale status|machines|connect|key|exit-node"

func cmdTailscale(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		helptext.Fprint(os.Stderr, tailscaleUsage+"\n")
		return 1
	}
	ctx := context.Background()
	switch args[0] {
	case "status":
		return tailscaleStatus(ctx, args[1:])
	case "machines":
		return tailscaleMachines(ctx, args[1:])
	case "connect":
		return tailscaleConnect(ctx, args[1:])
	case "key":
		return tailscaleKey(ctx, args[1:])
	case "exit-node":
		return tailscaleExitNode(ctx, args[1:])
	}
	return fail("tailscale: unknown subcommand %q (status|machines|connect|key|exit-node)", args[0])
}

func tailscaleStatus(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("tailscale status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of tailscale status:\n")
		helptext.Defaults(fs)
	}
	format := outfmt.Flag(fs)
	if fs.Parse(args) != nil {
		return 2
	}
	if err := outfmt.Check(*format); err != nil {
		return fail("%v", err)
	}
	st, err := tailscale.Status(ctx)
	if err != nil && !errors.Is(err, tailscale.ErrNotRunning) {
		return fail("tailscale status: %v", err)
	}
	if *format == outfmt.JSON {
		_ = outfmt.Write(os.Stdout, st)
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

func tailscaleMachines(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("tailscale machines", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of tailscale machines:\n")
		helptext.Defaults(fs)
	}
	format := outfmt.Flag(fs)
	if fs.Parse(args) != nil {
		return 2
	}
	if err := outfmt.Check(*format); err != nil {
		return fail("%v", err)
	}
	machines, err := tailscale.Machines(ctx)
	if err != nil {
		return fail("tailscale machines: %v", err)
	}
	names := make([]string, 0, len(machines))
	for _, m := range machines {
		names = append(names, m.Name)
	}
	writeDispatchCache(names) // `dispatch --host` completion reads this cache; never calls the network itself
	if *format == outfmt.JSON {
		_ = outfmt.Write(os.Stdout, machines)
		return 0
	}
	for _, m := range machines {
		fmt.Printf("%-40s %-10s online=%-5v source=%s\n", m.Name, m.OS, m.Online, m.Source)
	}
	return 0
}

func tailscaleConnect(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("tailscale connect", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of tailscale connect:\n")
		helptext.Defaults(fs)
	}
	authKey := fs.String("auth-key", "", "an auth key to connect with (tskey-auth-…)")
	keyEnv := fs.String("key-env", "", "read the auth key from this env var instead")
	exitNode := fs.Bool("exit-node", false, "also advertise this box as an exit node")
	if fs.Parse(args) != nil {
		return 2
	}
	key := *authKey
	if key == "" && *keyEnv != "" {
		key = strings.TrimSpace(os.Getenv(*keyEnv))
	}
	if key == "" {
		if st, err := tailscale.Status(ctx); err == nil && st.BackendState == "Running" {
			fmt.Printf("tailscale: already connected (%s)\n", st.DNSName)
			return 0
		}
		fmt.Fprintln(os.Stderr, "tailscale: no auth key given and not connected; run `claude-code configure` or pass --auth-key")
		return 2
	}
	if err := tailscale.Connect(ctx, key, *exitNode); err != nil {
		return fail("tailscale connect: %v", err)
	}
	st, _ := tailscale.Status(ctx)
	audit.Log(audit.TailscaleConnect, st.DNSName, "connect")
	fmt.Println("tailscale: connected")
	return 0
}

func tailscaleKey(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("tailscale key", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of tailscale key:\n")
		helptext.Defaults(fs)
	}
	ephemeral := fs.Bool("ephemeral", false, "the key's device is removed on logout/offline")
	preauth := fs.Bool("preauthorized", false, "new devices skip admin approval")
	reusable := fs.Bool("reusable", false, "the key can register more than one device")
	tags := fs.String("tags", "", "comma-separated ACL tags (tag:a,tag:b)")
	expiry := fs.String("expiry", "90d", "key lifetime: a Go duration, or Nd for N days")
	desc := fs.String("description", "", "a note shown in the admin console")
	if fs.Parse(args) != nil {
		return 2
	}
	exp, err := parseKeyExpiry(*expiry)
	if err != nil {
		return fail("tailscale key: --expiry: %v", err)
	}
	client, err := tailscale.NewClient()
	if err != nil {
		return fail("tailscale key: %v", err)
	}
	var tagList []string
	if *tags != "" {
		tagList = strings.Split(*tags, ",")
	}
	key, err := client.CreateAuthKey(ctx, tailscale.KeyOpts{
		Ephemeral: *ephemeral, Preauthorized: *preauth, Reusable: *reusable,
		Tags: tagList, Expiry: exp, Description: *desc,
	})
	if err != nil {
		// The admin API answers 404, not 403, when an OAuth client lacks the auth_keys scope.
		if strings.Contains(err.Error(), "HTTP 404") {
			return fail("tailscale key: %v\nhint: the OAuth client likely lacks Keys → Auth Keys: write, "+
				"and OAuth-minted keys need --tags tag:<x> (a tag in tagOwners); or use TAILSCALE_API_KEY", err)
		}
		return fail("tailscale key: %v", err)
	}
	audit.Log(audit.TailscaleKeyMint, "tailnet", "mint:"+strings.Join(tagList, ","))
	fmt.Println(key.Key) // the only thing on stdout, so it can be piped
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

func tailscaleExitNode(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("tailscale exit-node", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of tailscale exit-node:\n")
		helptext.Defaults(fs)
	}
	enable := fs.Bool("enable", false, "advertise this box as an exit node")
	disable := fs.Bool("disable", false, "stop advertising this box as an exit node")
	approve := fs.Bool("approve", false, "approve this box's exit-node routes via the admin API")
	if fs.Parse(args) != nil {
		return 2
	}
	if *enable && *disable {
		return fail("tailscale exit-node: --enable and --disable are mutually exclusive")
	}
	switch {
	case *enable:
		if err := tailscale.AdvertiseExitNode(ctx, true); err != nil {
			return fail("tailscale exit-node: %v", err)
		}
		audit.Log(audit.TailscaleExitNode, "self", "enable")
		fmt.Println("tailscale: advertising as an exit node")
	case *disable:
		if err := tailscale.AdvertiseExitNode(ctx, false); err != nil {
			return fail("tailscale exit-node: %v", err)
		}
		audit.Log(audit.TailscaleExitNode, "self", "disable")
		fmt.Println("tailscale: no longer advertising as an exit node")
	}
	if *approve {
		return tailscaleApproveExitNode(ctx)
	}
	if !*enable && !*disable {
		st, err := tailscale.Status(ctx)
		if err != nil && !errors.Is(err, tailscale.ErrNotRunning) {
			return fail("tailscale exit-node: %v", err)
		}
		fmt.Printf("option=%v approved=%v\n", st.ExitNodeOption, st.ExitNodeApproved)
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
	if err != nil && !errors.Is(err, tailscale.ErrNotRunning) {
		return fail("tailscale exit-node: %v", err)
	}
	devices, err := client.Devices(ctx)
	if err != nil {
		return fail("tailscale exit-node: %v", err)
	}
	var id string
	for _, d := range devices {
		if slices.ContainsFunc(d.Addresses, func(a string) bool { return slices.Contains(st.IPs, a) }) {
			id = d.ID
			break
		}
	}
	if id == "" {
		return fail("tailscale exit-node: could not match this node's address to a device (is it connected?)")
	}
	if err := client.ApproveExitNode(ctx, id); err != nil {
		return fail("tailscale exit-node: %v", err)
	}
	audit.Log(audit.TailscaleExitNode, st.DNSName, "approve")
	fmt.Println("tailscale: exit-node routes approved")
	return 0
}
