package remote

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestResolveAuto: the env value wins; one answering machine is chosen and cached; two or none is an error.
func TestResolveAuto(t *testing.T) {
	cases := []struct {
		name, env string
		peers     []string
		answer    []string
		want      string
		errHas    []string
	}{
		{"env wins without a probe", "wsl", []string{"a.ts.net", "b.ts.net"}, []string{"a.ts.net", "b.ts.net"}, "wsl", nil},
		{"single candidate", "", []string{"phone.ts.net", "box.ts.net"}, []string{"box.ts.net"}, "box.ts.net", nil},
		{"two candidates", "", []string{"a.ts.net", "b.ts.net"}, []string{"a.ts.net", "b.ts.net"}, "",
			[]string{"2 machines answer (a.ts.net, b.ts.net)", "claude-code vault edit", HostEnv}},
		{"none answers", "", []string{"phone.ts.net"}, nil, "", []string{"tried: phone.ts.net", HostEnv}},
		{"none online", "", nil, nil, "", []string{"none online", HostEnv}},
	}
	origEnv, origPeers, origAnswers := autoEnvHost, autoPeers, autoAnswers
	t.Cleanup(func() { autoEnvHost, autoPeers, autoAnswers = origEnv, origPeers, origAnswers })
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			probes := 0
			autoEnvHost = func() string { return c.env }
			autoPeers = func() ([]string, error) { probes++; return c.peers, nil }
			autoAnswers = func(h string) bool { return slices.Contains(c.answer, h) }
			got, err := ResolveAuto(Auto)
			if got != c.want || (err == nil) != (c.errHas == nil) {
				t.Fatalf("ResolveAuto = %q, %v; want %q", got, err, c.want)
			}
			for _, s := range c.errHas {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q lacks %q", err, s)
				}
			}
			if (c.env != "") != (probes == 0) {
				t.Errorf("tailnet read %d time(s) with env %q", probes, c.env)
			}
			if c.env == "" && c.want != "" { // the answer is cached: a second call never asks the tailnet
				autoPeers = func() ([]string, error) { return nil, errors.New("probed again") }
				if again, err := ResolveAuto(Auto); again != c.want || err != nil {
					t.Errorf("cached ResolveAuto = %q, %v; want %q", again, err, c.want)
				}
			}
		})
	}
	if got, err := ResolveAuto("wsl"); got != "wsl" || err != nil {
		t.Errorf("ResolveAuto(wsl) = %q, %v; a named host passes through", got, err)
	}
}
