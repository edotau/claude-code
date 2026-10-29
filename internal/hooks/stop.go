package hooks

import (
	"bytes"
	"io"
)

// Stop runs the context checkpoint and the memory harvest; a block from either blocks the stop, both messages shown.
func Stop(r io.Reader, stderr io.Writer) int {
	in := ParseInput(r)
	var cp, hv bytes.Buffer
	code := max(contextCheckpoint(in, &cp), sessionHarvest(in, &hv))
	_, _ = stderr.Write(cp.Bytes())
	_, _ = stderr.Write(hv.Bytes())
	return code
}
