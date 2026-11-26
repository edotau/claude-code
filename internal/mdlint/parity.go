package mdlint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"go.yaml.in/yaml/v4"
)

// deliberate maps a resolved mdsmith rule (or rule.path) to why it may differ from the converted markdownlint rules.
var deliberate = map[string]string{
	"ambiguous-emphasis":                 "analog of default-on MD037; the converter leaves it opt-in",
	"no-space-in-code-spans":             "analog of default-on MD038; the converter leaves it opt-in",
	"no-space-in-link-text":              "analog of default-on MD039; the converter leaves it opt-in",
	"single-h1":                          "analog of default-on MD025; the converter leaves it opt-in",
	"paragraph-readability":              "mdsmith-only, off: no VS Code analog",
	"max-file-length":                    "mdsmith-only, off: no VS Code analog",
	"token-budget":                       "mdsmith-only, off: no VS Code analog",
	"table-readability":                  "mdsmith-only, off: no VS Code analog",
	"empty-section-body":                 "mdsmith-only, gate-only: no VS Code analog",
	"cross-file-reference-integrity":     "mdsmith-only, gate-only: no VS Code analog",
	"no-duplicate-headings":              "MD024 siblings_only has no analog; VS Code-only",
	"list-indent":                        "MD005 stays VS Code-only; mdsmith's rule also covers disabled MD007",
	"no-multiple-blanks.settings.max":    "explicit 1 equals the MD012 default",
	"recipe-safety.settings.config-path": "a path, never comparable",
}

// noAnalog are converter notes that are expected: MD010 code_blocks is checked directly in Parity.
var noAnalog = []string{
	`MD010 option "code_blocks"`, `MD024 option "siblings_only"`, "MD060", "MD007 is disabled",
	"markdownlint enables these checks by default",
}

func allowed(key string) bool {
	_, ok := deliberate[key]
	_, rule := deliberate[strings.SplitN(key, ".", 2)[0]]
	return ok || rule
}

// Parity returns one drift line per mismatch between the editor rules, the ignore scope and mdsmith's rules.
func Parity(ctx context.Context, root string) ([]string, error) {
	if Mdsmith(root) == "" {
		return nil, ErrNoMdsmith
	}
	ctx = withLookupRoot(ctx, root)
	rulesBytes, err := os.ReadFile(filepath.Join(root, RulesFile))
	if err != nil {
		return nil, err
	}
	var rules map[string]any
	if err = json.Unmarshal(rulesBytes, &rules); err != nil {
		return nil, fmt.Errorf("%s: %w", RulesFile, err)
	}
	drift := optionDrift(rules)
	cfg, more, err := scope(root)
	if err != nil {
		return nil, err
	}
	drift = append(drift, more...)
	converted, notes, err := convertRules(ctx, rulesBytes)
	if err != nil {
		return nil, err
	}
	actual, err := resolve(ctx, root, "README.md")
	if err != nil {
		return nil, err
	}
	drift = append(append(drift, notes...), compareRules(converted, actual, "")...)
	for _, o := range cfg.Overrides {
		for _, glob := range o.Glob {
			more, err = checkOverride(ctx, root, glob, o.Rules, rules)
			if err != nil {
				return nil, err
			}
			drift = append(drift, more...)
		}
	}
	store, err := storeDrift(root)
	if err != nil {
		return nil, err
	}
	drift = append(drift, store...)
	slices.Sort(drift)
	return slices.Compact(drift), nil
}

type mdsmithConfig struct {
	Files     []string `yaml:"files"`
	Ignore    []string `yaml:"ignore"`
	Overrides []struct {
		Glob  []string       `yaml:"glob"`
		Rules map[string]any `yaml:"rules"`
	} `yaml:"overrides"`
}

// scope checks the cli2 ignores against .mdsmith.yml files and ignore.
func scope(root string) (mdsmithConfig, []string, error) {
	var cfg mdsmithConfig
	var drift []string
	raw, err := os.ReadFile(filepath.Join(root, CLI2File))
	if err != nil {
		return cfg, nil, err
	}
	var lines []string
	for l := range strings.SplitSeq(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "//") {
			lines = append(lines, l)
		}
	}
	var cli map[string]json.RawMessage
	if err = json.Unmarshal([]byte(strings.Join(lines, "\n")), &cli); err != nil {
		return cfg, nil, fmt.Errorf("%s: %w", CLI2File, err)
	}
	for key := range cli {
		if key != "ignores" {
			drift = append(drift, CLI2File+": unexpected "+key+" (rules live in "+RulesFile+")")
		}
	}
	var ignores []string
	if err = json.Unmarshal(cli["ignores"], &ignores); err != nil {
		return cfg, nil, fmt.Errorf("%s ignores: %w", CLI2File, err)
	}
	if raw, err = os.ReadFile(filepath.Join(root, MdsmithFile)); err != nil {
		return cfg, nil, err
	}
	if err = yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, nil, fmt.Errorf("%s: %w", MdsmithFile, err)
	}
	if !slices.Equal(cfg.Files, []string{"**/*.md"}) {
		drift = append(drift, MdsmithFile+": files must be [**/*.md]")
	}
	if !slices.Equal(slices.Compact(slices.Sorted(slices.Values(ignores))), slices.Compact(slices.Sorted(slices.Values(cfg.Ignore)))) {
		drift = append(drift, "ignore: markdownlint and mdsmith differ")
	}
	return cfg, drift, nil
}

func optionDrift(rules map[string]any) []string {
	var drift []string
	for _, w := range []struct {
		id, key string
		want    any
	}{{"MD013", "line_length", float64(Width)}, {"MD013", "stern", true}, {"MD010", "code_blocks", false}} {
		if obj, _ := rules[w.id].(map[string]any); obj[w.key] != w.want {
			drift = append(drift, fmt.Sprintf("%s.%s: expected %v", w.id, w.key, w.want))
		}
	}
	return drift
}

func storeDrift(root string) ([]string, error) {
	have, err := os.ReadFile(filepath.Join(root, "memory", RulesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err == nil && string(have) != string(StoreRules("memory")) {
		return []string{"memory/" + RulesFile + ": differs from StoreRules"}, nil
	}
	return nil, err
}

// checkOverride diffs the child .markdownlint.json mirroring an mdsmith override against its resolved rules.
func checkOverride(ctx context.Context, root, glob string, orule map[string]any, rules map[string]any) ([]string, error) {
	var parts []string
	for p := range strings.SplitSeq(filepath.ToSlash(glob), "/") {
		if strings.ContainsAny(p, "*?[") {
			break
		}
		parts = append(parts, p)
	}
	base := filepath.Join(parts...)
	if base == "" {
		return []string{glob + ": root-level override has no child " + RulesFile}, nil
	}
	childBytes, err := os.ReadFile(filepath.Join(root, base, RulesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return missingOverride(root, base, orule)
	}
	if err != nil {
		return nil, err
	}
	var overlay map[string]any
	if err = json.Unmarshal(childBytes, &overlay); err != nil {
		return nil, fmt.Errorf("%s/%s: %w", base, RulesFile, err)
	}
	merged := make(map[string]any, len(rules)+len(overlay))
	maps.Copy(merged, rules)
	maps.Copy(merged, overlay)
	delete(merged, "extends")
	mergedBytes, _ := json.Marshal(merged)
	want, notes, err := convertRules(ctx, mergedBytes)
	if err != nil {
		return nil, err
	}
	first := firstMarkdown(filepath.Join(root, base))
	if first == "" {
		return append(notes, base+": no Markdown file to resolve override"), nil
	}
	rel, _ := filepath.Rel(root, first)
	got, err := resolve(ctx, root, filepath.ToSlash(rel))
	if err != nil {
		return nil, err
	}
	return append(notes, compareRules(want, got, base+": ")...), nil
}

func missingOverride(root, base string, orule map[string]any) ([]string, error) {
	if base == "memory" {
		if _, err := os.Stat(filepath.Join(root, "memory")); errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
	}
	for key := range orule {
		if !allowed(key) {
			return []string{base + ": no " + RulesFile + " mirrors this override"}, nil
		}
	}
	return nil, nil
}

// convertRules runs mdsmith's markdownlint converter in a scratch dir; unexpected converter notes are drift.
func convertRules(ctx context.Context, rules []byte) (map[string]any, []string, error) {
	base := filepath.Join(paths.StateDir(), "mdlint")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp(base, "parity-")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	src := filepath.Join(dir, RulesFile)
	if err = errors.Join(os.WriteFile(src, rules, 0o600), os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Parity\n"), 0o600)); err != nil {
		return nil, nil, err
	}
	_, stderr, err := run(ctx, dir, "", "init", "--force", "--from-markdownlint="+src)
	if err != nil {
		return nil, nil, err
	}
	var notes []string
	for line := range strings.SplitSeq(string(stderr), "\n") {
		if _, note, ok := strings.Cut(line, "note:"); ok && !slices.ContainsFunc(noAnalog, func(a string) bool { return strings.Contains(note, a) }) {
			notes = append(notes, "converter: "+strings.TrimSpace(note))
		}
	}
	resolved, err := resolve(ctx, dir, "README.md")
	return resolved, notes, err
}

// resolve flattens `mdsmith kinds resolve` for file into rule.path -> value.
func resolve(ctx context.Context, dir, file string) (map[string]any, error) {
	out, _, err := run(ctx, dir, "", "kinds", "resolve", "--json", file)
	if err != nil {
		return nil, err
	}
	var data struct {
		Rules map[string]struct {
			Leaves []struct {
				Path  string `json:"path"`
				Value any    `json:"value"`
			} `json:"leaves"`
		} `json:"rules"`
	}
	if err = json.Unmarshal(out, &data); err != nil {
		return nil, fmt.Errorf("resolve %s: %w", file, err)
	}
	result := make(map[string]any)
	for name, rule := range data.Rules {
		for _, leaf := range rule.Leaves {
			result[name+"."+leaf.Path] = leaf.Value
		}
	}
	return result, nil
}

func compareRules(want, got map[string]any, prefix string) []string {
	var drift []string
	for _, k := range slices.Sorted(maps.Keys(want)) {
		if !allowed(k) && !disabledSetting(want, got, k) && !reflect.DeepEqual(want[k], got[k]) {
			drift = append(drift, fmt.Sprintf("%s%s: markdownlint says %v, .mdsmith.yml says %v", prefix, k, want[k], got[k]))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(got)) {
		if _, ok := want[k]; !ok && !allowed(k) && !disabledSetting(want, got, k) {
			drift = append(drift, fmt.Sprintf("%s%s: markdownlint says <none>, .mdsmith.yml says %v", prefix, k, got[k]))
		}
	}
	return drift
}

func disabledSetting(want, got map[string]any, key string) bool {
	rule, _, ok := strings.Cut(key, ".settings.")
	return ok && want[rule+".enabled"] == false && got[rule+".enabled"] == false
}

func firstMarkdown(dir string) string {
	var first string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, e error) error {
		if e == nil && !d.IsDir() && filepath.Ext(p) == ".md" {
			first = p
			return fs.SkipAll
		}
		return nil
	})
	return first
}
