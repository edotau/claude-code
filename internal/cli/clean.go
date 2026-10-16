package cli

import (
	"os"

	"github.com/edotau/claude-code/internal/clean"
)

func cmdClean(args []string) int { return clean.Run(args, os.Stdout, os.Stderr) }
