package configure

import (
	"bufio"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// copilot's Model row saves straight to the opus slot (pickCopilotModel): one model list, no tier step.
func TestModelRowCopilotSkipsTierPicker(t *testing.T) {
	reg := menuTestEnv(t)
	origChoices := modelChoices
	t.Cleanup(func() { modelChoices = origChoices })
	modelChoices = func(p *providers.Provider, claude bool) []string {
		return []string{"claude-opus-5.5", "claude-sonnet-5"}
	}

	d2 := &dialog{reg: reg, out: discardWriter{}}
	target, _ := modelTarget(d2, "copilot")
	if target == nil {
		t.Skip("no hub account to route the copilot Model row to")
	}
	wantSonnet := target.Models["sonnet"] // must survive untouched: copilot's row writes only the opus slot

	r := modelRow(d2, "copilot")
	if r == nil {
		t.Fatal("modelRow(copilot) = nil, want the Model row")
	}
	in := strings.NewReader("1\n") // numbered pick, non-TTY: row 1 = the first model id, no tier prompt first
	d2.o, d2.br = Options{In: in}, bufio.NewReader(in)
	if !r.run() {
		t.Fatal("copilot Model row run() = false")
	}
	p, err := reg.Get(target.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Models["opus"]; got != "claude-opus-5.5" {
		t.Errorf("%s opus slot = %q, want claude-opus-5.5 (one pick, no tier step)", target.Name, got)
	}
	if got := p.Models["sonnet"]; got != wantSonnet {
		t.Errorf("%s sonnet slot = %q, want unchanged %q: copilot's Model row must not write other slots", target.Name, got, wantSonnet)
	}
}

// codex (and every other hub agent) keeps the tier-then-model flow: the first pick selects a tier row
// (opus/sonnet/haiku), the second pick selects that tier's model id.
func TestModelRowCodexKeepsTierPicker(t *testing.T) {
	reg := menuTestEnv(t)
	origChoices := modelChoices
	t.Cleanup(func() { modelChoices = origChoices })
	modelChoices = func(p *providers.Provider, claude bool) []string { return []string{"gpt-5-codex"} }

	d2 := &dialog{reg: reg, out: discardWriter{}}
	target, _ := modelTarget(d2, "codex")
	if target == nil {
		t.Skip("no hub account to route the codex Model row to")
	}

	r := modelRow(d2, "codex")
	if r == nil {
		t.Fatal("modelRow(codex) = nil, want the Model row")
	}
	in := strings.NewReader("1\n1\n") // tier row 1 = opus, then model row 1 = the only choice
	d2.o, d2.br = Options{In: in}, bufio.NewReader(in)
	if !r.run() {
		t.Fatal("codex Model row run() = false")
	}
	p, err := reg.Get(target.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Models["opus"]; got != "gpt-5-codex" {
		t.Errorf("%s opus slot = %q, want gpt-5-codex (tier flow preserved)", target.Name, got)
	}
}
