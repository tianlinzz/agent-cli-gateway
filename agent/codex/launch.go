package codex

// BuildArgs builds the only supported persistent Codex transport.
func BuildArgs(_ Options, _ string) []string {
	return []string{"app-server", "--listen", "stdio://"}
}

func promptPreamble(systemPrompt, appendPrompt string) string {
	var sections []string
	if systemPrompt != "" {
		sections = append(sections, "Project system prompt:\n"+systemPrompt)
	}
	if appendPrompt != "" {
		sections = append(sections, "Additional project instructions:\n"+appendPrompt)
	}
	return joinSections(sections)
}
