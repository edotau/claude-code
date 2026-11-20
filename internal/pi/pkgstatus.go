package pi

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
)

// PkgStatus is one pinned package (or a stale store slot) as found on disk; Problem is empty when healthy.
type PkgStatus struct {
	Spec, Problem       string
	Kinds               []string
	ShipsCode, Approved bool
	Installed, Stale    bool
	VersionMismatch     bool
}

// PackageStatus reads the record's pins and the store without installing, removing, locking or touching the network.
func PackageStatus() ([]PkgStatus, error) {
	rec, err := agentconfig.Settings("pi")
	if err != nil {
		return nil, err
	}
	wanted, err := ParsePackages(rec.Extras[extraPackages])
	if err != nil {
		return nil, err
	}
	approved, err := ParsePackages(rec.Extras[extraPackagesApproved])
	if err != nil {
		return nil, err
	}
	return statusIn(packageStore(), paths.ConfigDir(), wanted, approved)
}

func statusIn(store, configDir string, wanted, approved []Package) ([]PkgStatus, error) {
	var out []PkgStatus
	keep := map[string]bool{}
	for _, p := range wanted {
		keep[slotName(p)] = true
		out = append(out, pkgStatus(store, configDir, p, approved))
	}
	ents, err := os.ReadDir(store)
	if err != nil && !errors.Is(err, fs.ErrNotExist) { // an unreadable store is a problem even with nothing pinned
		return out, fmt.Errorf("pi package store: %w", err)
	}
	for _, e := range ents {
		if !slotRe.MatchString(e.Name()) || keep[e.Name()] {
			continue
		}
		spec, _ := os.ReadFile(filepath.Join(store, e.Name(), specMarker))
		s := strings.TrimSpace(string(spec))
		if s == "" {
			s = e.Name()
		}
		out = append(out, PkgStatus{Spec: s, Installed: true, Stale: true, Problem: "stale slot not in the list; " +
			"`claude-code clean` removes it once no pi run is live"})
	}
	return out, nil
}

func pkgStatus(store, configDir string, p Package, approved []Package) PkgStatus {
	st := PkgStatus{Spec: p.Spec, Approved: Approved(p.Spec, approved)}
	dir := p.LocalPath(configDir)
	if p.Kind != KindLocal {
		slot := filepath.Join(store, slotName(p))
		if b, err := os.ReadFile(filepath.Join(slot, specMarker)); err != nil || string(b) != p.Spec {
			st.Problem = "not installed yet; installs on the next pi launch"
			return st
		}
		dir = packageDir(slot, p)
	}
	rv, err := ReviewPackage(dir)
	if err != nil {
		st.Problem = err.Error()
		return st
	}
	st.Installed, st.ShipsCode = true, rv.ShipsCode
	st.Kinds = reviewKinds(rv)
	if err := checkPin(p, dir, rv); err != nil { // the same name, version and commit check a launch runs
		st.VersionMismatch, st.Problem = true, err.Error()
	} else if rv.ShipsCode && !st.Approved {
		st.Problem = "ships code and is not approved"
	}
	return st
}

func reviewKinds(rv Review) []string {
	var out []string
	for k, v := range map[string][]string{"extensions": rv.Extensions, "skills": rv.Skills, "prompts": rv.Prompts, "themes": rv.Themes} {
		if len(v) > 0 {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}
