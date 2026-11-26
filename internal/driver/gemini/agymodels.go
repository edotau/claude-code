package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// agyModelsTTL is how long one `agy models` read is reused; a refresh ignores it.
const agyModelsTTL = 24 * time.Hour

// ParseAgyModels is the ids in `agy models` output: a header line, then one "<id>\t<label>" row per model.
func ParseAgyModels(out []byte) []string {
	var ids []string
	for line := range strings.SplitSeq(string(out), "\n") {
		id, _, tab := strings.Cut(line, "\t")
		if id = strings.TrimSpace(id); tab && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// AgyModels lists the models agy serves: a Databricks gateway profile's served Gemini list, else `agy models`.
func AgyModels(ctx context.Context, p *providers.Provider, refresh bool) ([]string, error) {
	if p != nil && p.Preset == providers.PresetDatabricks {
		return agyServedGemini(p, time.Now()), nil
	}
	ttl := agyModelsTTL
	if refresh {
		ttl = 0
	}
	valid := func(ids []string) ([]byte, error) { return json.Marshal(ids) }
	cache := paths.DiskTTL[[]string]{
		Name: "agy-models", Path: filepath.Join(paths.StateDir(), "agy-models.json"), TTL: ttl,
		Decode: func(b []byte) (ids []string, err error) { return ids, json.Unmarshal(b, &ids) }, Encode: valid,
	}
	return cache.Get(ctx, runAgyModels)
}

// agyModelsTimeout bounds one `agy models` run; a var so a test can shrink it.
var agyModelsTimeout = 15 * time.Second

func runAgyModels(ctx context.Context) ([]string, error) {
	bin, err := paths.AgentBin("agy")
	if err != nil {
		return nil, err
	}
	return execAgyModels(ctx, bin)
}

// execAgyModels runs `bin models` under agyModelsTimeout in its own process group, so a hung child tree dies too.
func execAgyModels(ctx context.Context, bin string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, agyModelsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "models")
	proc.OwnGroup(cmd)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, errors.New("agy models timed out")
	}
	if err != nil {
		return nil, err
	}
	return ParseAgyModels(out), nil
}
