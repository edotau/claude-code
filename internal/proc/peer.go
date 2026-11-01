package proc

import "errors"

// ErrPeerUnsupported marks a platform with no socket peer-credential call.
var ErrPeerUnsupported = errors.New("peer credentials unsupported on this platform")
