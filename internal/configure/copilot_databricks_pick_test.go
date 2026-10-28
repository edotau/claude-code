package configure

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/copilotreg"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vendoragent"
)

func TestPickCopilotModelDatabricksStoresCanonicalID(t *testing.T) {
	reg := menuTestEnv(t)
	cat := vendoragent.CopilotCatalog{
		Databricks: true,
		Source:     "dev serving endpoints",
		Rows: []copilotreg.Endpoint{
			{ID: "databricks-kimi-k3", Tested: true},
			{ID: "databricks-gpt-6-1-sol"},
		},
	}
	orig := copilotSourceFn
	t.Cleanup(func() { copilotSourceFn = orig })
	copilotSourceFn = func(context.Context, *providers.Registry, string) (vendoragent.CopilotCatalog, error) {
		return cat, nil
	}

	in := strings.NewReader("\n")
	var out bytes.Buffer
	d := &dialog{reg: reg, o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	pickCopilotModel(d)
	shown := out.String()
	for _, id := range []string{"databricks-kimi-k3", "databricks-gpt-6-1-sol"} {
		if !strings.Contains(shown, id) {
			t.Errorf("serving endpoint %q missing from picker:\n%s", id, shown)
		}
	}
	choice := 0
	for line := range strings.SplitSeq(shown, "\n") {
		var n int
		var id string
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "%d) %s", &n, &id); err == nil && id == "databricks-kimi-k3" {
			choice = n
			if !strings.Contains(line, "tested") {
				t.Errorf("kimi row missing tested marker: %q", line)
			}
		}
	}
	if choice == 0 {
		t.Fatalf("numbered kimi row missing from picker:\n%s", shown)
	}

	in = strings.NewReader(fmt.Sprintf("%d\n", choice))
	out.Reset()
	d = &dialog{reg: reg, o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	pickCopilotModel(d)
	if got, _ := providers.CopilotModel(reg); got != "kimi-k3" {
		t.Errorf("COPILOT_MODEL = %q, want kimi-k3; picker output:\n%s", got, out.String())
	}

	_, rows := appendDatabricksCopilotRows(cat, "kimi-k3", nil)
	if rows[0][0] != "databricks-kimi-k3" || !strings.Contains(rows[0][1], "current") {
		t.Errorf("canonical current model not marked on endpoint row: %v", rows[0])
	}
}
