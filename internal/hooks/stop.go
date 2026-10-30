package hooks

import "bytes"

// memoryUpdateStop runs the context checkpoint and the memory update; a block from either blocks the stop, both
// messages shown.
func memoryUpdateStop(in *Input) Result {
	var cp, hv bytes.Buffer
	code := max(contextCheckpoint(in, &cp), sessionUpdate(in, &hv))
	return exitResult(code, cp.String()+hv.String())
}
