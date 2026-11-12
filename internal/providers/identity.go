package providers

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Identity modes for a paired Databricks profile: which credential a launch uses.
const (
	IdentityAuto = "auto" // user login first, the service principal when it is rejected or needs a login
	IdentityUser = "user" // user login only
	IdentitySP   = "sp"   // service principal only, no browser
	// IdentityHeadless marks a headless dispatch with no --as: on an auto row, the SP first, the user login as fallback.
	IdentityHeadless = "headless"
)

// headlessAs maps IdentityHeadless to no override; other values pass.
func headlessAs(as string) string {
	if as == IdentityHeadless {
		return ""
	}
	return as
}

// ResolveIdentity picks the mode: flagAs (--as) > the profile's stored identity > auto (user when unpaired).
func ResolveIdentity(profile, flagAs string) (string, error) {
	mode := cmp.Or(flagAs, profile, IdentityAuto)
	switch mode {
	case IdentityAuto, IdentityUser, IdentitySP:
		return mode, nil
	}
	return "", fmt.Errorf("identity %q must be auto, user or sp", mode)
}

type identityKey struct{}

// WithIdentity carries a launch's --as value on the credential request's context.
func WithIdentity(ctx context.Context, as string) context.Context {
	return context.WithValue(ctx, identityKey{}, as)
}

// IdentityFrom is the identity WithIdentity put on ctx, "" when none.
func IdentityFrom(ctx context.Context) string {
	as, _ := ctx.Value(identityKey{}).(string)
	return as
}

type notesKey struct{}

// WithNotes routes ctx's credential notes (the service-principal fallback line) to sink instead of stderr.
func WithNotes(ctx context.Context, sink func(string)) context.Context {
	return context.WithValue(ctx, notesKey{}, sink)
}

// note prints msg to ctx's WithNotes sink, else to spNoteOut.
func note(ctx context.Context, msg string) {
	if sink, ok := ctx.Value(notesKey{}).(func(string)); ok {
		sink(msg)
		return
	}
	fmt.Fprintln(spNoteOut, msg)
}

// TakeAs removes --as user|sp (or --as=V) from args and validates it; it stops at a bare "--".
func TakeAs(args []string) (string, []string, error) {
	var as string
	var rest []string
	seen := false
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(args[i], "=")
		switch {
		case args[i] == "--":
			rest = append(rest, args[i:]...)
			i = len(args)
		case name != "--as":
			rest = append(rest, args[i])
		case hasVal:
			as, seen = val, true
		case i+1 < len(args):
			i++
			as, seen = args[i], true
		default:
			return "", nil, errors.New("--as needs user or sp")
		}
	}
	if seen && !slices.Contains([]string{IdentityUser, IdentitySP}, as) {
		return "", nil, fmt.Errorf("--as %q must be user or sp", as)
	}
	return as, rest, nil
}
