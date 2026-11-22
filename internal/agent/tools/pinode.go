package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
)

// PiNodeBinDir is the bin dir of the Node Pi runs on: "" when the PATH node is new enough, else the newest nvm or mise one.
func PiNodeBinDir(ctx context.Context) (string, error) {
	pathNode, _ := depLookPath("node")
	return pickPiNode(ctx, pathNode, nvmDir(), miseDir(), false)
}

// PiNodePathEnv is the child-only PATH=<node bin>:$PATH entry for launching Pi, or nil when the PATH node serves.
func PiNodePathEnv(ctx context.Context) []string {
	dir, err := PiNodeBinDir(ctx)
	if err != nil {
		return nil
	}
	return pathEnv(dir)
}

func pathEnv(dir string) []string {
	if dir == "" {
		return nil
	}
	return []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}
}

func nvmDir() string {
	if d := os.Getenv("NVM_DIR"); d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".nvm")
	}
	return ""
}

func nodeOK(ctx context.Context, node string) bool {
	if node == "" {
		return false
	}
	c, ok := textutil.CompareVersions(parseVersion(versionOutput(ctx, node, versionProbeTimeout, "--version")), piMinNode)
	return ok && c >= 0
}

// pickPiNode is the node bin dir ("" = PATH's); withNPM also requires an npm beside the picked node.
func pickPiNode(ctx context.Context, pathNode, nvm, mise string, withNPM bool) (string, error) {
	if nodeOK(ctx, pathNode) && (!withNPM || npmIn("") != "") {
		return "", nil
	}
	for _, n := range nodeCandidates(nvm, mise) {
		if nodeOK(ctx, n) && (!withNPM || npmIn(filepath.Dir(n)) != "") {
			return filepath.Dir(n), nil
		}
	}
	return "", errNoPiNode
}

// nodeCandidates globs nvm and mise installs, one entry per real node (aliases dropped), newest first, non-versions last.
func nodeCandidates(nvm, mise string) []string {
	var cands []string
	if nvm != "" {
		cands, _ = filepath.Glob(filepath.Join(nvm, "versions", "node", "v*", "bin", "node"))
	}
	if mise != "" {
		m, _ := filepath.Glob(filepath.Join(mise, "installs", "node", "*", "bin", "node"))
		cands = append(cands, m...)
	}
	ver, seen := map[string]string{}, map[string]bool{}
	cands = slices.DeleteFunc(cands, func(n string) bool {
		real, err := filepath.EvalSymlinks(n)
		if err != nil {
			real = n
		}
		if seen[real] {
			return true
		}
		seen[real] = true
		if v := nodeVer(real); v != "" {
			if _, ok := textutil.CompareVersions(v, v); ok {
				ver[n] = v
			}
		}
		return false
	})
	slices.SortStableFunc(cands, func(a, b string) int {
		if (ver[a] == "") != (ver[b] == "") {
			if ver[a] == "" {
				return 1
			}
			return -1
		}
		c, _ := textutil.CompareVersions(ver[b], ver[a])
		return c
	})
	return cands
}

// miseDir is mise's data dir; Windows' %LOCALAPPDATA%\mise layout has no bin/ so its glob matches nothing.
func miseDir() string {
	if d := os.Getenv("MISE_DATA_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "mise")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "mise")
	}
	return ""
}

// npmIn is the npm in dir (PATH's for ""), or "" when absent; LookPath follows links and adds Windows' npm.cmd.
func npmIn(dir string) string {
	if dir == "" {
		npm, _ := depLookPath("npm")
		return npm
	}
	npm, _ := exec.LookPath(filepath.Join(dir, "npm"))
	return npm
}

// nodeVer is the version in .../vX.Y.Z/bin/node (nvm's v prefix dropped).
func nodeVer(node string) string {
	return strings.TrimPrefix(filepath.Base(filepath.Dir(filepath.Dir(node))), "v")
}
