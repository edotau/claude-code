package databricks

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

// `databricks identity` writes through the menu's SaveIdentity and refuses sp on a row with no pair or no secret.
func TestIdentityVerbPinsAndRefuses(t *testing.T) {
	for _, c := range []struct {
		name, stored   string
		paired, secret bool
		args           []string
		code           int
		out, want      string
	}{
		{"sp, paired", "auto", true, true, []string{"-p", "dbx-sp", "sp"}, 0, "dbx-sp: identity auto → sp · SP sp-client", "sp"},
		{"user keeps the pair", "sp", true, true, []string{"-p", "dbx-sp", "user"}, 0, "identity sp → user", "user"},
		{"dry-run writes nothing", "auto", true, true, []string{"-p", "dbx-sp", "sp", "--dry-run"}, 0, "would set", "auto"},
		{"sp, unpaired", "auto", false, false, []string{"-p", "dbx-sp", "sp"}, 1, "identity sp needs a service principal client id", "auto"},
		{"sp, no secret", "auto", true, false, []string{"-p", "dbx-sp", "sp"}, 1, "`claude-code vault set DATABRICKS_DBX_SP_M2M`", "auto"},
		{"bad mode", "auto", true, true, []string{"-p", "dbx-sp", "bogus"}, 2, "", "auto"},
		{"no profile", "auto", true, true, []string{"sp"}, 2, "", "auto"},
		{"not a databricks profile", "auto", true, true, []string{"-p", "nope", "sp"}, 1, `"nope" is not a Databricks profile`, "auto"},
	} {
		spLoginRow(t, c.stored, c.paired, c.secret)
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return identity(c.args) })
		reg := mustLoad(t)
		if got := reg.Providers["dbx-sp"].Identity; code != c.code || got != c.want || !strings.Contains(stdout+stderr, c.out) {
			t.Errorf("%s: exit %d identity %q output %q; want exit %d identity %q containing %q",
				c.name, code, got, stdout+stderr, c.code, c.want, c.out)
		}
	}
}
