package doctor

import (
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/pi"
)

// piPackagesDetail renders the pi packages row; any problem makes it a soft failure.
func piPackagesDetail() (bool, string) {
	sts, err := pi.PackageStatus()
	if err != nil {
		return false, err.Error()
	}
	if len(sts) == 0 {
		return true, "none"
	}
	ok, parts := true, make([]string, 0, len(sts))
	for _, s := range sts {
		approved := "n/a"
		if s.ShipsCode {
			approved = map[bool]string{true: "yes", false: "no"}[s.Approved]
		}
		code := map[bool]string{true: "yes", false: "no"}[s.ShipsCode]
		d := fmt.Sprintf("%s kinds=%s code=%s approved=%s", s.Spec, strings.Join(s.Kinds, ","), code, approved)
		if s.Problem != "" {
			ok, d = false, d+" ("+s.Problem+")"
		}
		parts = append(parts, d)
	}
	return ok, strings.Join(parts, "; ")
}
