package profile

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

const useAcctDoc = `{"providers":{
"dbx-dev":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net","cliProfile":"dev"},
"dbx-qa":{"preset":"databricks","host":"https://adb-2.azuredatabricks.net","cliProfile":"qa"},
"dbx-prod":{"preset":"databricks","host":"https://adb-3.azuredatabricks.net","cliProfile":"prod"},
"acme":{"kind":"openai","base_url":"https://acme.example/v1","auth":{"type":"none"}}},
"profiles":{"active":"","sets":{"DEFAULT":{
"session":{"account":"dbx-dev","model":"m1"},
"codex":{"account":"dbx-qa","model":"dbx-dev:gpt-x"},
"api":{"account":"acme","model":"dbx-dev:keep-not"},
"gemini":{"account":"acme","model":"g"},
"opencode":{"model":"dbx-qa:z","models":{"opus":"dbx-dev:o","sonnet":"acme:s"}}}}}}`

func useAcctReg(t *testing.T) *providers.Registry {
	t.Helper()
	seed(t, useAcctDoc)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestUseAccountRepointsPresetRows(t *testing.T) {
	reg := useAcctReg(t)
	if _, _, err := UseAccount(true, reg, "DEFAULT", "dbx-prod"); err != nil {
		t.Fatal(err)
	}
	got, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Profiles.Active != "DEFAULT" {
		t.Errorf("active = %q", got.Profiles.Active)
	}
	set := got.Profiles.Sets["DEFAULT"]
	if set["session"].Account != "dbx-prod" || set["codex"].Account != "dbx-prod" {
		t.Errorf("preset rows not moved: %+v %+v", set["session"], set["codex"])
	}
	if set["codex"].Model != "dbx-prod:gpt-x" {
		t.Errorf("provider:model spec not moved: %q", set["codex"].Model)
	}
	if o := set["opencode"]; o.Model != "dbx-prod:z" || o.Models["opus"] != "dbx-prod:o" || o.Models["sonnet"] != "acme:s" {
		t.Errorf("opencode row = %+v", o)
	}
	if set["gemini"].Account != "acme" || set["api"].Account != "acme" {
		t.Errorf("non-preset row moved: %+v %+v", set["gemini"], set["api"])
	}
}

func TestUseAccountErrors(t *testing.T) {
	reg := useAcctReg(t)
	before := rawDoc(t)
	for _, c := range []struct{ name, account, want string }{
		{"DEFAULT", "nope", ""},
		{"DEFAULT", "acme", "no preset"},
		{"missing", "dbx-prod", "no profile set"},
	} {
		_, _, err := UseAccount(true, reg, c.name, c.account)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("UseAccount(%q,%q) err = %v, want contains %q", c.name, c.account, err, c.want)
		}
	}
	if rawDoc(t) != before {
		t.Fatal("a failed UseAccount changed the vault")
	}
}

func TestUseAccountDryRunWritesNothing(t *testing.T) {
	reg := useAcctReg(t)
	before := rawDoc(t)
	oldDoc, newDoc, err := UseAccount(false, reg, "DEFAULT", "dbx-prod")
	if err != nil {
		t.Fatal(err)
	}
	if rawDoc(t) != before {
		t.Fatal("dry run changed the vault")
	}
	if string(oldDoc) == string(newDoc) || !strings.Contains(string(newDoc), "dbx-prod") {
		t.Errorf("dry run did not plan the change:\n%s", newDoc)
	}
}
