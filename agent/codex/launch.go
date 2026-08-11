package codex

import "fmt"

// BuildArgs builds a fresh or resumed `codex exec --json` invocation.
func BuildArgs(options Options, threadID string) []string {
	opts := NormalizeOptions(options)
	resume := threadID != ""
	args := []string{"exec", "--skip-git-repo-check"}
	if resume {
		args = []string{"exec", "resume", "--skip-git-repo-check"}
		args = append(args, "-c", `sandbox_mode="danger-full-access"`, "-c", `approval_policy="never"`)
	} else {
		args = append(args, "--sandbox", "danger-full-access", "-c", `approval_policy="never"`)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ModelProvider != "" {
		args = append(args, "-c", fmt.Sprintf("model_provider=%q", opts.ModelProvider))
	}
	if opts.BaseURL != "" {
		args = append(args, "-c", fmt.Sprintf("openai_base_url=%q", opts.BaseURL))
	}
	if opts.ReasoningEffort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", opts.ReasoningEffort))
	}
	if resume {
		args = append(args, threadID, "--json", "-")
	} else {
		args = append(args, "--json", "--cd", opts.WorkDir, "-")
	}
	return args
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
