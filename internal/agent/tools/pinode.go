package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
)

// PiNodeBinDir is the bin dir of the Node Pi runs on: "" when the PATH node is new enough, else the newest nvm one.
func PiNodeBinDir(ctx context.Context) (string, error) {
	pathNode, _ := depLookPath("node")
	return pickPiNode(ctx, pathNode, nvmDir())
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

func pickPiNode(ctx context.Context, pathNode, nvm string) (string, error) {
	if nodeOK(ctx, pathNode) {
		return "", nil
	}
	if nvm != "" {
		cands, _ := filepath.Glob(filepath.Join(nvm, "versions", "node", "v*", "bin", "node"))
		slices.SortFunc(cands, func(a, b string) int {
			c, _ := textutil.CompareVersions(nodeVer(b), nodeVer(a))
			return c
		})
		for _, n := range cands {
			if nodeOK(ctx, n) {
				return filepath.Dir(n), nil
			}
		}
	}
	return "", errNoPiNode
}

// nodeVer is the version in .../versions/node/vX.Y.Z/bin/node.
func nodeVer(node string) string {
	return strings.TrimPrefix(filepath.Base(filepath.Dir(filepath.Dir(node))), "v")
}
