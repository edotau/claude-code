package hooks

import (
	"bytes"
	"io"
)

// Stop runs the context checkpoint, the memory harvest, then the formatter; a block from either of the first two
// blocks the stop, with both messages shown when both fire.
func Stop(r io.Reader, stderr io.Writer) int {
	in := ParseInput(r)
	var cp, hv bytes.Buffer
	code := max(contextCheckpoint(in, &cp), sessionHarvest(in, &hv))
	_, _ = stderr.Write(cp.Bytes())
	_, _ = stderr.Write(hv.Bytes())
	stopFormat(in)
	return code
}
