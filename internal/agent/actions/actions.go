// Package actions is the one table of typed harness actions; Go builds each claude-code argv from typed arguments.
package actions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/agent/wire"
)

// Spec is one action: its MCP/Pi description and schema, and the builder of its claude-code sub-argv.
type Spec struct {
	Name, Description string
	Schema            map[string]any
	Argv              func(args json.RawMessage) ([]string, error)
}

type gateArgs struct {
	Scope []string `json:"scope,omitempty" jsonschema:"description=clean relative dirs to gate; omit for the whole tree"`
}

type diffArgs struct {
	Paths []string `json:"paths,omitempty" jsonschema:"description=files or dirs to scope the diff to"`
	Check bool     `json:"check,omitempty" jsonschema:"description=also run the Go and docs gates on those paths"`
}

// scopeEntry is a gate scope directory's allowed spelling; IsLocal also bars absolute and ".." paths.
var scopeEntry = regexp.MustCompile(`^[A-Za-z0-9._/@+-]+$`)

// Table is every typed action, in tool-list order.
var Table = []Spec{
	{Name: "gate", Description: "Run the scoped quality gate (worktree gate --run).",
		Schema: schemaOf[gateArgs](), Argv: gateArgv},
	{Name: "diff", Description: "List changed files as JSON, scoped by paths; check adds the Go and docs gates.",
		Schema: schemaOf[diffArgs](), Argv: diffArgv},
}

// Argv builds the sub-argv of the named action; the second result is false for an unknown name.
func Argv(name string, args json.RawMessage) ([]string, bool, error) {
	for _, s := range Table {
		if s.Name == name {
			argv, err := s.Argv(args)
			return argv, true, err
		}
	}
	return nil, false, nil
}

func schemaOf[T any]() map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(wire.SchemaFor[T]()), &m); err != nil {
		panic("actions: " + err.Error()) // SchemaFor emits valid JSON
	}
	return m
}

func decode(name string, raw json.RawMessage, into any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if len(raw) > 64<<10 || bytes.TrimSpace(raw)[0] != '{' {
		return fmt.Errorf("%s: arguments must be an object of at most 64 KiB", name)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("%s: invalid arguments: %w", name, err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("%s: trailing data after arguments", name)
	}
	return nil
}

func gateArgv(raw json.RawMessage) ([]string, error) {
	var a gateArgs
	if err := decode("gate", raw, &a); err != nil {
		return nil, err
	}
	if len(a.Scope) > 256 {
		return nil, errors.New("gate: at most 256 scope entries")
	}
	for _, s := range a.Scope {
		if strings.HasPrefix(s, "-") || !filepath.IsLocal(s) || !scopeEntry.MatchString(s) {
			return nil, fmt.Errorf("gate: scope entry %q is not a clean relative directory", s)
		}
	}
	argv := []string{"worktree", "gate", "--run"}
	if len(a.Scope) > 0 {
		argv = append(argv, "--scope", strings.Join(a.Scope, ","))
	}
	return argv, nil
}

func diffArgv(raw json.RawMessage) ([]string, error) {
	var a diffArgs
	if err := decode("diff", raw, &a); err != nil {
		return nil, err
	}
	if len(a.Paths) > 256 {
		return nil, errors.New("diff: at most 256 paths")
	}
	argv := []string{"agents", "diff", "--format", "json"}
	for _, p := range a.Paths {
		if p == "" || strings.HasPrefix(p, "-") || strings.ContainsAny(p, ",\x00") {
			return nil, fmt.Errorf("diff: path %q is empty or looks like a flag", p)
		}
		argv = append(argv, "--path", p)
	}
	if a.Check {
		argv = append(argv, "--check")
	}
	return argv, nil
}
