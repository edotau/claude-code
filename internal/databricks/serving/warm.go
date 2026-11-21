package serving

import (
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/serving"
)

// WarmServed saves infos as profile's listing unless the file is newer or infos is empty.
func WarmServed(profile string, infos []providers.ModelInfo, at time.Time) error {
	if len(infos) == 0 {
		return nil
	}
	s := serving.Served{Profile: profile, FetchedAt: at.UTC()}
	for _, m := range infos {
		s.Models = append(s.Models, serving.ServedModel{ID: m.ID, Task: m.Task, Ready: m.Ready, APITypes: m.APITypes,
			ModelClass: m.ModelClass, Context: m.ContextToken, Output: m.OutputToken})
	}
	// a file dated past clockSkew ahead of at is distrusted and replaced, or it would block every later warm
	return serving.Save(s, func(cur serving.Served) bool { return cur.FetchedAt.After(at) && cur.Fresh(at) })
}
