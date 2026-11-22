// Command rtkwrap is the RTK_WRAP shim: each internal/rtkwrap Dir entry links here and argv[0] names the command.
package main

import (
	"os"

	"github.com/edotau/claude-code/internal/rtkwrap"
)

func main() { os.Exit(rtkwrap.Main(os.Args)) }
