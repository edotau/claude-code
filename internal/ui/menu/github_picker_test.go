package menu

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	sdk "github.com/github/copilot-sdk/go"
)

func TestPickCopilotModelsMarksChangedAndReloadsRegistry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("catalog fixture requires a POSIX shell")
	}
	reg := menuTestEnv(t)
	fakeCopilotBin(t)
	script := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
  case "$line" in Content-Length:*) n=${line#*: }; n=${n%%?};; esac
  [ -z "${line%%?}" ] || continue
  body=$(/usr/bin/head -c "$n")
  id=$(printf '%%s' "$body" | /usr/bin/sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  r='{"jsonrpc":"2.0","id":"'"$id"'","result":{"protocolVersion":%d,"models":[{"id":"catalog-model","name":"Catalog Model"}]}}'
  printf 'Content-Length: %%s\r\n\r\n%%s' "${#r}" "$r"
done
`, sdk.GetSDKProtocolVersion())
	if err := os.WriteFile(filepath.Join(paths.BinDir(), "copilot"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader("2\n2\n2\n")
	var out bytes.Buffer
	d := &dialog{reg: reg, o: Options{In: in}, br: bufio.NewReader(in), out: &out}
	pickCopilotModels(d)
	if !d.changed {
		t.Fatalf("saved fallback models must trigger config regeneration: %s", &out)
	}
	if d.reg == reg {
		t.Fatal("saved fallback models must reload the dialog registry")
	}
	for _, alias := range []string{models.Opus, models.Sonnet, models.Haiku} {
		if got := d.reg.Providers["personal"].Models[alias]; got != "catalog-model" {
			t.Errorf("reloaded %s = %q, want catalog-model", alias, got)
		}
	}
}
