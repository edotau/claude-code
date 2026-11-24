package assets

type BriefFrame struct {
	Agent, OwnedLine, Verify, Report, Task, ReportsDir string
	Rtk                                                bool // the rtk shell-output line shows
}

var briefTemplates = loadTemplates("files/brief/*.tmpl")

func renderBriefText(name string, input any) string { return renderFrom(briefTemplates(), name, input) }

func BriefBody(in BriefFrame) string {
	return renderBriefText("brief.tmpl", tmplFields{
		"Agent": in.Agent, "OwnedLine": in.OwnedLine, "Verify": in.Verify, "Report": in.Report, "Task": in.Task,
		"ReportsDir": in.ReportsDir, "Rtk": in.Rtk,
	})
}

func ReviewLoop() string { return string(read("files/brief/review-loop.txt")) }

func LoopPlanner(goal string, round, limit, maxParts, maxID, digestChars int, digest string) string {
	return renderBriefText("loop-planner.tmpl", tmplFields{
		"Goal": goal, "Round": round, "Max": limit, "MaxParts": maxParts, "MaxID": maxID,
		"DigestChars": digestChars, "Digest": digest,
	})
}

func ReviewNeutral() string { return string(read("files/brief/review-neutral.txt")) }

func ReviewAdversarial(clean string) string {
	return renderBriefText("review-adversarial.tmpl", tmplFields{"Clean": clean})
}

func TakeoverBrief(agentName, cut, task string, partialBytes int, partial string) string {
	return renderBriefText("takeover.tmpl", tmplFields{
		"Agent": agentName, "Cut": cut, "Task": task, "Bytes": partialBytes, "Partial": partial,
	})
}

func ExplorePrompt(question string, web bool) string {
	return renderBriefText("explore.tmpl", tmplFields{"Question": question, "Web": web})
}
