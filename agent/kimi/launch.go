package kimi

// BuildArgs returns the only supported persistent Kimi transport.
func BuildArgs(opts Options, _, _ string) []string {
	opts = NormalizeOptions(opts)
	args := make([]string, 0, 3)
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	return append(args, "acp")
}
