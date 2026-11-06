package cli

// cmdGemini is the gemini agent verb; launch, ask, resume, status, result, cancel, setup, models and help are agentVerb's job.
func cmdGemini(args []string) int {
	return agentVerb("gemini", args)
}
