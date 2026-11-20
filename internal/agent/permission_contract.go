package agent

import (
	"cmp"
	"errors"

	"github.com/edotau/claude-code/internal/agent/retry"
)

// CheckCapabilities refuses an unenforceable --no-tools or permission feature; other unsupported flags run, dropped.
func CheckCapabilities(name string, req RunOptions) error {
	r, err := Lookup(name)
	if err == nil {
		err = capabilityRefusal(r.Caps(), name, req)
	}
	if err != nil {
		return retry.Fatal(name, err)
	}
	return nil
}

// capabilityRefusal is the first capability c cannot honor for req, or nil.
func capabilityRefusal(c Caps, name string, req RunOptions) error {
	if err := cmp.Or(refusePersona(c, name, req.Persona), refusePermissions(c, name, req.Permissions)); err != nil {
		return err
	}
	switch {
	case req.NoTools && c.NoTools == None:
		return errors.New("unsupported capability: --no-tools")
	case req.Tools && req.NoTools:
		return errors.New("incompatible capabilities: --tools and --no-tools")
	case req.ReadOnly && c.ReadOnly == Emulated && c.Permissions == None:
		return errors.New("--read-only requires permission enforcement")
	}
	return nil
}
