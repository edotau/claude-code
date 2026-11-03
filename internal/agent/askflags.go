package agent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// FormatStreamJSON is the --format value for the newline-delimited event stream.
const FormatStreamJSON = "stream-json"

// ValidateAs validates --as; it applies to claude only, other agents follow the profile's identity setting.
func ValidateAs(as, ag string) error {
	if as == "" {
		return nil
	}
	if as != providers.IdentityUser && as != providers.IdentitySP {
		return fmt.Errorf("--as %q must be user or sp", as)
	}
	if ag != "claude" {
		return fmt.Errorf("--as applies to claude only; %s follows the profile's identity setting", ag)
	}
	return nil
}

// ValidateAskFlags checks --format and --fallback against their allowed values.
func ValidateAskFlags(format, fallback string) error {
	if !slices.Contains([]string{helptext.Text, helptext.JSON, FormatStreamJSON}, format) {
		return fmt.Errorf("--format %q unknown (text|json|stream-json)", format)
	}
	if !slices.Contains(router.FallbackModes, fallback) {
		return fmt.Errorf("--fallback %q unknown (%s)", fallback, strings.Join(router.FallbackModes, "|"))
	}
	return nil
}

// AskExclusive rejects flag pairs that cannot be combined.
func AskExclusive(background bool, job string, tools, noTools bool) error {
	if background && job != "" {
		return errors.New("--background and --job are mutually exclusive")
	}
	if tools && noTools {
		return errors.New("--tools and --no-tools are mutually exclusive")
	}
	return nil
}

// AskPositional is the task words; on resume the first positional sets session.
func AskPositional(positional []string, resume bool, session *string) ([]string, error) {
	if !resume {
		return positional, nil
	}
	if len(positional) < 1 {
		return nil, errors.New("resume needs a session id: <agent> resume [flags] <id> <task>")
	}
	*session = positional[0]
	return positional[1:], nil
}

// AskReselect re-resolves the agent after --job/--retry; it returns the new selector and agent.
func AskReselect(ref, ag string, name *string, persona **Schema, defs map[string]*Schema) (string, string, error) {
	if *name != ag { // --job/--retry replaced the agent: resolve the record's (or re-typed) value instead
		ref = *name
	}
	rest, p, err := SelectAgent(ref, defs) // a --retry re-parse puts the raw --agent value back
	if err != nil {
		return ref, ag, err
	}
	*name = rest
	if p != nil { // an explicit persona beats the record's
		*persona = p
	}
	return ref, rest, nil
}

// ReadFramedPrompt reads the prompt up to the NUL that ends it and returns the reader, whose remaining EOF is the cancel signal.
func ReadFramedPrompt(in io.Reader) (*bufio.Reader, string, error) {
	rd := bufio.NewReader(in)
	p, err := rd.ReadString(0)
	if err != nil {
		return nil, "", fmt.Errorf("--stdin-framed: stdin ended before the NUL that ends the prompt: %w", err)
	}
	return rd, strings.TrimSuffix(p, "\x00"), nil
}

// ResolveAskPrompt reads stdin when the prompt is empty or the literal "-".
func ResolveAskPrompt(prompt string) (string, error) {
	if prompt != "" && prompt != "-" {
		return prompt, nil
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(raw), nil
}
