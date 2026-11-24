package assets

// HookLaneQueueLine is one merge-queue line the main session sees every prompt until the lane merges.
func HookLaneQueueLine(lane, branch, reason, detail, next string) string {
	return renderHook("lane-queue-line.tmpl", tmplFields{
		"Lane": lane, "Branch": branch, "Reason": reason, "Detail": detail, "Next": next,
	})
}

func HookLaneQueueMore(count int) string {
	return renderHook("lane-queue-more.tmpl", tmplFields{"Count": count})
}

// HookLaneMerged is the notice text for a lane the lane-merge worker promoted.
func HookLaneMerged(lane, detail string, removed bool) string {
	return renderHook("lane-merged.tmpl", tmplFields{"Lane": lane, "Detail": detail, "Removed": removed})
}
