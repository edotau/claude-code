package worktree

import "os"

// Names lists the worktree dirs under cwd's repo root (~/worktrees/<repo>) — a plain listing, fast for Tab.
func Names(cwd string) []string {
	repo, err := ResolveRepo(cwd)
	if err != nil {
		return nil
	}
	ents, err := os.ReadDir(repo.Root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}
