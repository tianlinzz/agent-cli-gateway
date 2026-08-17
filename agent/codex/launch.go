package codex

// BuildArgs builds the only supported persistent Codex transport.
func BuildArgs(_ Options, _ string) []string {
	return []string{"app-server", "--listen", "stdio://"}
}
