package kimi

// BuildArgs returns the only supported persistent Kimi transport.
func BuildArgs(_ Options, _, _ string) []string { return []string{"acp"} }
