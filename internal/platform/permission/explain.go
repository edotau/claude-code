package permission

// Denial identifies a blocked call, the layer and source that refused it, and its remedy.
type Denial struct {
	Call   Rule
	Layer  string
	Rule   Rule
	Source string
	Remedy string
	Replay string
}
