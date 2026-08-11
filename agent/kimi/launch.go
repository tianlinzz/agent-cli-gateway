package kimi

// BuildArgs builds one non-interactive Kimi turn.
func BuildArgs(options Options, prompt, resumeID string) []string {
	opts := NormalizeOptions(options)
	var args []string
	if opts.Flags.Print {
		args = append(args, "--print")
	}
	args = append(args, "--output-format", "stream-json")
	switch opts.Mode {
	case "plan":
		args = append(args, "--plan")
	case "quiet":
		args = append(args, "--quiet")
	}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.WorkDir != "" {
		args = append(args, "--work-dir", opts.WorkDir)
	}
	return append(args, "--prompt", prompt)
}
