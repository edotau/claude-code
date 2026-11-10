package ask

import (
	"context"
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Catalog is `<agent> catalog`: the agent's own catalog (a ModelsProvider lists through models), or a no-catalog note.
func Catalog(name, profile, format string, models func(args []string) int) int {
	s, ok := agent.LookupSchema(name)
	if !ok || !s.External() {
		return helptext.Fail("unknown agent %q", name)
	}
	if name == "copilot" {
		if done, code := copilotSource(profile, format); done {
			return code
		}
	}
	switch {
	case s.CLI.ModelsProvider != "":
		reg, err := providers.Load()
		if err != nil {
			return helptext.Fail("%v", err)
		}
		n, err := s.ModelsProfile(reg)
		if err != nil {
			return helptext.Fail("%v", err)
		}
		return models([]string{"--live", "--profile", n, "--format", format})
	case len(s.CLI.ModelsArgv) > 0:
		return Launch(name, s.CLI.ModelsArgv)
	}
	fmt.Printf("%s: no model catalog\n", name)
	return 0
}

// copilotSource prints a Databricks profile's catalog (done) or, in text, the GitHub model source line.
func copilotSource(profile, format string) (bool, int) {
	reg, err := providers.Load()
	if err != nil {
		return true, helptext.Fail("%v", err)
	}
	cat, err := copilot.CopilotModelSource(context.Background(), reg, profile)
	if err == nil && cat.Databricks {
		err = cat.Write(os.Stdout, format)
	}
	if err != nil {
		return true, helptext.Fail("copilot catalog: %v", err)
	}
	if cat.Databricks {
		return true, 0
	}
	if format == helptext.Text {
		fmt.Println(cat.Source)
	}
	return false, 0
}
