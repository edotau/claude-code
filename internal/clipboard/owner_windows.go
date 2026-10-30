package clipboard

import "os"

// ownerUID has no uid on Windows; the SSH bridge check then refuses the socket.
func ownerUID(os.FileInfo) (int, bool) { return 0, false }
