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

// PiNodeBinDir is the bin dir of the Node Pi runs on: "" when the PATH node is new enough, else the newest nvm one.
func PiNodeBinDir(ctx context.Context) (string, error) {
	pathNode, _ := depLookPath("node")
	return pickPiNode(ctx, pathNode, nvmDir(), false)
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
func pickPiNode(ctx context.Context, pathNode, nvm string, withNPM bool) (string, error) {
	if nodeOK(ctx, pathNode) && (!withNPM || npmIn("") != "") {
		return "", nil
	}
	if nvm != "" {
		cands, _ := filepath.Glob(filepath.Join(nvm, "versions", "node", "v*", "bin", "node"))
		slices.SortFunc(cands, func(a, b string) int {
			c, _ := textutil.CompareVersions(nodeVer(b), nodeVer(a))
			return c
		})
		for _, n := range cands {
			if nodeOK(ctx, n) && (!withNPM || npmIn(filepath.Dir(n)) != "") {
				return filepath.Dir(n), nil
			}
		}
	}
	return "", errNoPiNode
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

// nodeVer is the version in .../versions/node/vX.Y.Z/bin/node.
func nodeVer(node string) string {
	return strings.TrimPrefix(filepath.Base(filepath.Dir(filepath.Dir(node))), "v")
}
