package statusline

import "testing"

func TestPSParentTTY(t *testing.T) {
	t.Run("native", func(t *testing.T) { testPSParentTTYNative(t, psParentTTY) })
	t.Run("invalid pid", func(t *testing.T) { testPSParentTTYInvalidPID(t, psParentTTY) })
	t.Run("context", func(t *testing.T) { testPSParentTTYContext(t, psParentTTY) })
}
