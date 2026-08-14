package config

import (
	"fmt"
	"log/slog"
	"strings"
)

// CommandSpec is an agent CLI command in argv form (O-F14). The canonical
// TOML representation is an array:
//
//	command = ["/opt/agent tools/codex", "app-server"]
//
// A plain string is still accepted for one deprecation window so existing
// deployments keep working, but it is tokenized with a shell-like splitter
// (whitespace separation with single/double-quote grouping and backslash
// escapes) instead of naive whitespace splitting, so quoted paths with spaces
// survive. A future release rejects the string form.
type CommandSpec struct {
	argv []string
	// legacyString records that the TOML value was the deprecated string
	// form, so LoadGateway can warn once per agent.
	legacyString bool
}

// Argv returns the command as an argv slice. It is nil when no command is
// configured (the adapter resolves its default binary).
func (c CommandSpec) Argv() []string { return c.argv }

// IsLegacyString reports whether the command was configured in the
// deprecated single-string form.
func (c CommandSpec) IsLegacyString() bool { return c.IsSet() && c.legacyString }

// IsSet reports whether a command was configured at all.
func (c CommandSpec) IsSet() bool { return len(c.argv) > 0 }

// UnmarshalTOML accepts either an argv array of strings (canonical) or a
// single string (deprecated; split shell-like). Any other shape is a config
// error.
func (c *CommandSpec) UnmarshalTOML(value any) error {
	switch typed := value.(type) {
	case string:
		argv, err := SplitCommandString(typed)
		if err != nil {
			return fmt.Errorf("config: command %q: %w", typed, err)
		}
		c.argv = argv
		c.legacyString = true
		return nil
	case []any:
		argv := make([]string, 0, len(typed))
		for i, item := range typed {
			token, ok := item.(string)
			if !ok {
				return fmt.Errorf("config: command array element %d is a %T, want a string", i, item)
			}
			argv = append(argv, token)
		}
		if err := validateArgv(argv); err != nil {
			return err
		}
		c.argv = argv
		c.legacyString = false
		return nil
	case []string:
		argv := append([]string(nil), typed...)
		if err := validateArgv(argv); err != nil {
			return err
		}
		c.argv = argv
		c.legacyString = false
		return nil
	case nil:
		c.argv = nil
		c.legacyString = false
		return nil
	default:
		return fmt.Errorf("config: command must be an argv array of strings or a single string, got %T", value)
	}
}

func validateArgv(argv []string) error {
	if len(argv) == 0 {
		return nil
	}
	if strings.TrimSpace(argv[0]) == "" {
		return fmt.Errorf("config: command executable must not be empty")
	}
	return nil
}

// warnLegacyCommand emits the one-time deprecation warning for an agent still
// configured with the string command form.
func warnLegacyCommand(agent string, command CommandSpec) {
	slog.Warn("config: agents.<id>.command as a single string is deprecated; use an argv array (quoted paths with spaces stay one token)",
		"agent", agent, "argv", strings.Join(command.Argv(), " "))
}

// SplitCommandString tokenizes a legacy single-string command the way a
// POSIX shell would split a command line (without command substitution or
// variable expansion): whitespace separates tokens, single quotes preserve
// everything literally, double quotes preserve everything except backslash
// escapes, and a backslash escapes the next character. Unlike
// strings.Fields, a quoted path with spaces stays a single token.
func SplitCommandString(command string) ([]string, error) {
	var (
		argv     []string
		token    strings.Builder
		inToken  bool
		quote    rune
		escaped  bool
		balanced = true
	)
	flush := func() {
		if inToken {
			argv = append(argv, token.String())
			token.Reset()
			inToken = false
		}
	}
	for _, r := range command {
		switch {
		case escaped:
			token.WriteRune(r)
			escaped = false
		case quote != 0:
			switch {
			case r == '\\' && quote == '"':
				escaped = true
			case r == quote:
				quote = 0
			default:
				token.WriteRune(r)
			}
		case r == '\\':
			escaped = true
			inToken = true
		case r == '\'' || r == '"':
			quote = r
			inToken = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			token.WriteRune(r)
			inToken = true
		}
	}
	if quote != 0 || escaped {
		balanced = false
	}
	flush()
	if !balanced {
		return nil, fmt.Errorf("unterminated quote or escape")
	}
	if err := validateArgv(argv); err != nil {
		return nil, err
	}
	return argv, nil
}
