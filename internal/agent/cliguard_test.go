package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/paths/pathstest"
)

func TestCLIVersionAtParsesFakeBinary(t *testing.T) {
	for _, tc := range []struct{ out, want string }{
		{"codex-cli 0.171.0", "0.171.0"}, {"v0.166.2", "0.166.2"}, {"garbage", ""},
	} {
		bin := filepath.Join(t.TempDir(), "codex")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '"+tc.out+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := CLIVersionAt(t.Context(), bin); got != tc.want {
			t.Errorf("CLIVersionAt(%q) = %q, want %q", tc.out, got, tc.want)
		}
	}
}

func TestDriftNotePolicies(t *testing.T) {
	for _, tc := range []struct {
		have   string
		policy VersionPolicy
		want   string
		pol    VersionPolicy
	}{
		{"0.171.4", "", "", VersionWarn},
		{"", VersionRequire, "", VersionRequire},
		{"0.166.0", "", "codex: CLI 0.166.0 differs from verified 0.171 (warn)", VersionWarn},
		{"0.166.0", VersionRequire, "codex: CLI 0.166.0 differs from verified 0.171 (require)", VersionRequire},
		{"0.166.0", VersionIgnore, "", VersionIgnore},
	} {
		got, pol := driftNote("codex", tc.have, CLI{Verified: "0.171", VersionPolicy: tc.policy})
		if got != tc.want || pol != tc.pol {
			t.Errorf("driftNote(%q, %q) = %q, %q; want %q, %q", tc.have, tc.policy, got, pol, tc.want, tc.pol)
		}
	}
}

func TestRequireDriftRefusesBothPaths(t *testing.T) {
	hosttest.HermeticEnv(t)
	pathstest.FakeVendor(t, "pi", "echo 'pi 0.166.0'")
	pathstest.FakeVendor(t, "codex", "echo 'codex-cli 0.166.0'")
	registryMu.Lock()
	addRow(Schema{Name: "pi", CLI: CLI{Binary: "pi", Verified: "0.171", VersionPolicy: VersionRequire}})
	i, _ := slices.BinarySearchFunc(rosterRows, "codex", func(r Schema, n string) int { return strings.Compare(r.Name, n) })
	orig := rosterRows[i]
	rosterRows[i].CLI.Verified, rosterRows[i].CLI.VersionPolicy = "0.171", VersionRequire
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		defer registryMu.Unlock()
		rosterRows = slices.DeleteFunc(rosterRows, func(r Schema) bool { return r.Name == "pi" })
		j, _ := slices.BinarySearchFunc(rosterRows, "codex", func(r Schema, n string) int { return strings.Compare(r.Name, n) })
		rosterRows[j] = orig
	})
	_, err := resolveExplicit(t.Context(), RouteReq{Agent: "pi"})
	var pe *PreflightError
	if !errors.As(err, &pe) || !strings.Contains(pe.Finding.Detail, "CLI 0.166.0 differs from verified 0.171 (require)") {
		t.Fatalf("resolveExplicit err = %v, want a drift PreflightError", err)
	}
	rc := routeCtx{pf: &Preflight{}}
	hop := func(n string) Dispatch { return Dispatch{Agent: namedStub{stubEngine{name: n}}} }
	c := guardCLIVersion(rc, Chain{Dispatches: []Dispatch{hop("pi"), hop("codex"), hop("claude")}})
	if len(c.Dispatches) != 1 || c.Dispatches[0].Agent.Name() != "claude" || len(rc.pf.Failed) != 2 {
		t.Errorf("chain = %+v, failed %v; want only claude kept and two findings", c.Dispatches, rc.pf.Failed)
	}
	if len(c.Skipped) != 2 || !strings.Contains(c.Skipped[1], "codex: CLI 0.166.0 differs") {
		t.Errorf("skipped = %v, want both drifts named", c.Skipped)
	}
}

type namedStub struct{ stubEngine }

func (namedStub) Run(context.Context, RunOptions, wire.Sink) (wire.Result, error) {
	return wire.Result{}, nil
}
