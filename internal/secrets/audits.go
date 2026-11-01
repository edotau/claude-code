package secrets

import (
	"fmt"
	"io"
	"os"

	"github.com/edotau/claude-code/internal/providers"
)

// AuditOpts selects the credential audits doctor --scan/--git/--secrets runs.
type AuditOpts struct {
	Scan, Git, Vault bool
	Targets          []string
	Rev              string
	Since            int
	VaultDoctor      func(io.Writer) bool // the vault health checks; injected because launch imports secrets
}

// RunAudits runs the requested audits in order, writing their reports to w; the worst exit wins.
func RunAudits(w io.Writer, o AuditOpts) int {
	rc, sep := ExitClean, ""
	if o.Scan {
		rc = max(rc, (&Scanner{Out: w}).ScanConfigs(o.Targets))
		sep = "\n"
	}
	if o.Git {
		fmt.Fprint(w, sep)
		dir, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "claude-code: doctor --git: %v\n", err)
			return 1
		}
		rc, sep = max(rc, ScanGit(w, dir, o.Rev, o.Since)), "\n"
	}
	if o.Vault {
		fmt.Fprint(w, sep)
		if !o.VaultDoctor(w) {
			rc = max(rc, ExitFindings)
		}
	}
	return rc
}

// CredSource names where p's credential resolves from (vault|env|command|auth.file|cli|none|missing).
func CredSource(p *providers.Provider) (source, note string) {
	st := Resolve(p)
	if st.Source == "none" && p.Auth.Method() != providers.AuthNone {
		return "missing", st.Note
	}
	return st.Source, st.Note
}
