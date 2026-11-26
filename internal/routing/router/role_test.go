package router

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/dev/contract"
)

func TestParseProviderMode(t *testing.T) {
	for in, want := range map[string]ProviderMode{"": ModeRotate, "rotate": ModeRotate, "exclusive": ModeExclusive} {
		if got, err := ParseProviderMode(in); err != nil || got != want {
			t.Errorf("ParseProviderMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseProviderMode("bogus"); !errors.As(err, new(ProviderModeError)) {
		t.Errorf("bogus: want a ProviderModeError, got %v", err)
	}
}

func TestRoleCells(t *testing.T) {
	provs := []ClaudeProvider{
		{Name: "anthropic", Models: []string{"opus", "sonnet", "haiku", "fable"}},
		{Name: "databricks-dev", Models: []string{"opus", "sonnet", "haiku"}},
	}
	owner := []RoleCell{{"anthropic", "opus"}, {"databricks-dev", "opus"}}
	fleet := []RoleCell{{"anthropic", "sonnet"}, {"anthropic", "haiku"}, {"databricks-dev", "sonnet"}, {"databricks-dev", "haiku"}}
	if got := RoleCells(RoleOwner, provs); !reflect.DeepEqual(got, owner) {
		t.Errorf("owner cells = %v", got)
	}
	if got := RoleCells(RoleFleet, provs); !reflect.DeepEqual(got, fleet) {
		t.Errorf("fleet cells = %v", got)
	}
}

func TestFableHint(t *testing.T) {
	if !strings.Contains(FableHint("fable"), "anthropic") || FableHint("opus") != "" {
		t.Error("FableHint must name anthropic for Fable only")
	}
}

func TestEveryContractRoleHasModels(t *testing.T) {
	for _, r := range contract.Roles() {
		if len(Role(r).Models()) == 0 {
			t.Errorf("contract role %q has no router model tiers", r)
		}
	}
}
