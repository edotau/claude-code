package vault

import "strconv"

// Schema is the layout number stamped in meta/schema (0 when unset).
func (t *Tx) Schema() int {
	s, _ := t.Get(Meta, "schema")
	n, _ := strconv.Atoi(s)
	return n
}

// SetSchema stamps meta/schema.
func (t *Tx) SetSchema(n int) error { return t.Put(Meta, "schema", strconv.Itoa(n)) }
