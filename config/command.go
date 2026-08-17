package config

import (
	"fmt"
	"log/slog"
	"strings"
)

// CommandSpec is an agent CLI command in argv form (O-F14). The canonical —
// and only argument-capable — TOML representation is an array:
//
//	command = ["/opt/agent tools/codex", "app-server"]
//
// A plain string is accepted for one deprecation window and interpreted as
// ONE executable path (the whole string becomes argv[0]), never as shell
// syntax: `"codex --flag"` names an executable literally called
// `codex --flag`, which will not resolve — arguments must be migrated to the
// array form. A future release rejects the string form outright.
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

// UnmarshalTOML accepts either an argv array of strings (canonical, the only
// form that can carry arguments) or a single string (deprecated; the whole
// string is one executable path). Any other shape is a config error.
func (c *CommandSpec) UnmarshalTOML(value any) error {
	switch typed := value.(type) {
	case string:
		if err := validateArgv([]string{typed}); err != nil {
			return fmt.Errorf("config: command %q: %w", typed, err)
		}
		c.argv = []string{typed}
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
		return fmt.Errorf("config: command must be an argv array of strings or a single executable path, got %T", value)
	}
}

func validateArgv(argv []string) error {
	if len(argv) == 0 {
		return nil
	}
	if strings.TrimSpace(argv[0]) == "" {
		return fmt.Errorf("config: command executable must not be empty or whitespace")
	}
	return nil
}

// warnLegacyCommand emits the one-time deprecation warning for an agent still
// configured with the string command form.
func warnLegacyCommand(agent string, command CommandSpec) {
	slog.Warn("config: agents.<id>.command as a single string is deprecated; the string is interpreted as ONE executable path (not shell syntax) — migrate arguments to the argv array form",
		"agent", agent, "executable", command.Argv()[0])
}
