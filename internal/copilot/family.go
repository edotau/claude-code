package copilot

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/catalog"
)

var modelFamilies = []string{"gpt", "gemini", "grok", "claude", "kimi", "mai"}

// ResolveModelWord reads only the sealed catalog snapshot, so a label never waits on the network.
func (copilotEngine) ResolveModelWord(_ context.Context, model string) (string, error) {
	if !familyWord(model) {
		return model, nil
	}
	var ids []string
	for _, m := range effortModels() {
		if m.Enabled {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return model, nil
	}
	return reviewModel(model, "", ids)
}

// ColdModelWord is the error for a family word the empty snapshot left unresolved.
func (copilotEngine) ColdModelWord(model string) error {
	if !familyWord(model) {
		return nil
	}
	return fmt.Errorf("copilot model %q needs the catalog snapshot: run `claude-code copilot catalog`, then retry", model)
}

func modelFamily(id string) string {
	if _, bare, ok := strings.Cut(id, ":"); ok {
		id = bare
	}
	id = catalog.ID(id)
	for _, family := range modelFamilies {
		if strings.HasPrefix(id, family+"-") {
			return family
		}
	}
	return ""
}

func familyWord(model string) bool {
	return slices.Contains(modelFamilies, model)
}

// ReviewModel preserves ordinary model validation and defaults when no author family is known.
func ReviewModel(ctx context.Context, model, author string) (string, error) {
	if !familyWord(model) && (model != "" || modelFamily(author) == "") {
		return model, nil
	}
	ids, err := copilotModelIDs(ctx)
	if err != nil && model == "" {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return reviewModel(model, author, ids)
}

func reviewModel(model, author string, ids []string) (string, error) {
	if model != "" && !familyWord(model) {
		return model, nil
	}
	family := modelFamily(author)
	if model == "" && family == "" {
		return "", nil
	}
	var candidates []catalog.Candidate
	originals := map[string]string{}
	for _, id := range ids {
		if reviewFamilyEligible(model, family, modelFamily(id)) {
			key := strings.ReplaceAll(id, ".", "-")
			candidates = append(candidates, catalog.Candidate{Model: key})
			originals[key] = id
		}
	}
	choice := catalog.Choose(candidates, "deep", false)
	if len(choice.Ranked) > 0 {
		return originals[choice.Ranked[0].Model], nil
	}
	if len(candidates) > 0 {
		return originals[candidates[0].Model], nil
	}
	if model != "" {
		return "", unknownGithubModel(model, "github", ids)
	}
	return "", nil
}

func reviewFamilyEligible(model, authorFamily, family string) bool {
	if model != "" {
		return family == model
	}
	return family != "" && family != authorFamily && family != "claude"
}
