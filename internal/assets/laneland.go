package assets

// HookLaneQueueLine is one land-queue line the main session sees every prompt until the lane lands.
func HookLaneQueueLine(lane, branch, reason, detail, next string) string {
	return renderHook("lane/queue-line", tmplFields{
		"Lane": lane, "Branch": branch, "Reason": reason, "Detail": detail, "Next": next,
	})
}

func HookLaneQueueMore(count int) string {
	return renderHook("lane/queue-more", tmplFields{"Count": count})
}
