package crossgen

import "strings"

// skipNames are Claude-Code-only sources never projected; adding a name here also prunes an already-synced copy.
var skipNames = map[string]bool{
	"approval-gate": true, // describes the CC gate chain; meaningless as a Gemini skill
}

// Banners stamped on every generated SKILL.md; prune deletes only dirs whose banner is the first body line.
const (
	agentSkillBannerPrefix = "<!-- Synced from .claude/agents/"
	realSkillBannerPrefix  = "<!-- Synced from .claude/skills/"
)

var generatedBannerPrefixes = []string{agentSkillBannerPrefix, realSkillBannerPrefix}

// projectedNames is the ONE definition of what a run writes; read by the write side and prune's keep-set.
func projectedNames(agents, skills []Source) map[string]bool {
	out := make(map[string]bool, len(agents)+len(skills))
	realSkill := realSkillNames(skills)
	for _, s := range skills {
		if !skipNames[s.dirName] {
			out[s.Name()] = true
		}
	}
	for _, a := range agents {
		if !skipNames[a.dirName] && !realSkill[a.Name()] {
			out[a.Name()] = true
		}
	}
	return out
}

// realSkillNames is every skill's output name, skip-list ignored (a skipped skill still owns its name).
func realSkillNames(skills []Source) map[string]bool {
	out := make(map[string]bool, len(skills))
	for _, s := range skills {
		out[s.Name()] = true
	}
	return out
}

// skillDescription is the cleaned frontmatter description, falling back to the name.
func skillDescription(src Source) string {
	if raw := strings.TrimSpace(toString(src.frontmatter["description"])); raw != "" {
		return cleanDescription(raw)
	}
	return src.Name()
}

// transformAgentSkill renders an agent as a Gemini SKILL.md: (name, content).
func transformAgentSkill(agent Source) (string, string) {
	name := agent.Name()
	fields := []orderedField{{"name", name}, {"description", skillDescription(agent)}}
	body := stripMemorySection(agent.body)
	if truthy(agent.frontmatter["readonly"]) {
		body = "**Note: this skill describes a read-only workflow; do not modify files.**\n\n" + body
	}
	banner := agentSkillBannerPrefix + agent.dirName + ".md; do not edit by hand. -->\n\n"
	return name, buildFrontmatterFile(fields, banner+body)
}

// transformRealSkill renders a skill as a normalized Gemini SKILL.md: (name, content).
func transformRealSkill(skill Source) (string, string) {
	name := skill.Name()
	fields := []orderedField{{"name", name}, {"description", skillDescription(skill)}}
	banner := realSkillBannerPrefix + skill.dirName + "/SKILL.md; do not edit by hand. -->\n\n"
	return name, buildFrontmatterFile(fields, banner+strings.TrimLeft(skill.body, "\n"))
}
