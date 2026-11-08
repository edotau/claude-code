package job_test

// The external test package registers opencode in the roster; an in-package import would cycle through hooks.
import _ "github.com/edotau/claude-code/internal/opencode"
