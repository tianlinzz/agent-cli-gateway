package claudecode

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BuildArgs builds the native persistent stream-json arguments. appendFile is
// the already-created append-system-prompt file owned by the Session.
func BuildArgs(opts Options, appendFile string) []string {
	opts = NormalizeOptions(opts)
	args := []string{
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--permission-prompt-tool", "stdio",
		"--replay-user-messages",
		"--verbose",
	}
	if opts.Mode != "" && opts.Mode != "default" {
		args = append(args, "--permission-mode", opts.Mode)
	}
	if opts.ResumeID != "" {
		args = append(args, "--resume", opts.ResumeID)
	}
	if len(opts.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(opts.AllowedTools, ","))
	}
	if len(opts.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(opts.DisallowedTools, ","))
	}
	if opts.SystemPrompt != "" {
		args = append(args, "--system-prompt", opts.SystemPrompt)
	}
	if appendFile != "" {
		args = append(args, "--append-system-prompt-file", appendFile)
	}
	if opts.ReasoningEffort != "" {
		args = append(args, "--effort", opts.ReasoningEffort)
	}
	if opts.MaxContextTokens > 0 {
		args = append(args, "--max-context-tokens", strconv.Itoa(opts.MaxContextTokens))
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	return args
}

func writeAppendPromptFile(content string) (string, error) {
	dir := filepath.Join(os.TempDir(), "agent-gateway-prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, "claude-system-*.md")
	if err != nil {
		return "", err
	}
	name := file.Name()
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		os.Remove(name)
		return "", err
	}
	if err := file.Chmod(0o644); err != nil {
		file.Close()
		os.Remove(name)
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}
