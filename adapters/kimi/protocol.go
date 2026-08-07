// Kimi protocol helpers — migrated from the upstream in-repo agent/kimi
// package (originally cc-connect) and adapted to the new runtime contract.
//
// This file holds the pure, unit-testable protocol pieces of the Kimi CLI:
// `--help` flag-surface probing, stream-json argument building, prompt
// serialization, resume-id extraction and redaction helpers. Nothing here
// imports the old core/IM packages or net/http.
package kimi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// kimiFlagSupport records which optional CLI flags the locally installed Kimi
// binary understands. It is populated once at adapter construction by probing
// `kimi --help` so that buildArgs can adapt to CLI versions that have added
// or removed flags.
//
// Why this exists:
//
//	The newer Kimi Code CLI removed the standalone `--print` flag — passing it
//	now produces: `error: unknown option '--print' (Did you mean --prompt?)`.
//	The older kimi-cli still requires `--print` for `--output-format` to take
//	effect. We probe the help text once and adapt accordingly. See #1456.
type kimiFlagSupport struct {
	Print bool
}

// probeKimiFlags runs `<cmd> --help` with a short timeout and returns the
// detected flag-support set. If the probe fails (binary missing, timeout,
// non-zero exit, unrecognisable output) the returned struct has all fields
// false — i.e. we conservatively assume the newer CLI surface, which matches
// the direction Kimi Code CLI is moving and avoids the hard-failure mode of
// passing `--print` to a CLI that no longer accepts it.
func probeKimiFlags(parent context.Context, cmd string, timeout time.Duration) kimiFlagSupport {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	c := exec.CommandContext(ctx, cmd, "--help")
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &out
	if err := c.Run(); err != nil {
		slog.Debug("kimi: flag probe failed, assuming modern CLI surface",
			"cmd", cmd, "error", err)
		return kimiFlagSupport{}
	}

	flags := parseKimiHelpFlags(out.String())
	support := kimiFlagSupport{
		Print: flags["--print"],
	}
	slog.Debug("kimi: flag probe complete", "cmd", cmd, "support", support)
	return support
}

// parseKimiHelpFlags scans the output of `kimi --help` and returns the set of
// long flag names (`--xxx`) it advertises. It handles the two common option-
// table layouts Kimi has shipped:
//
//	Typer/click box style:   "│ --print            Run in print mode (...) │"
//	Standard click style:    "  -p, --prompt TEXT  User prompt (...)"
//	Slash aliases:           "  --thinking/--no-thinking   Enable thinking."
//
// To stay robust against future layout tweaks the parser only treats a line
// as a flag-definition line when one of its first two whitespace-separated
// tokens starts with `--`; description prose later in the line is ignored.
func parseKimiHelpFlags(helpText string) map[string]bool {
	flags := make(map[string]bool)
	for _, rawLine := range strings.Split(helpText, "\n") {
		line := strings.TrimLeft(rawLine, " \t│|*")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// Look at up to the first two tokens so `-p, --prompt …` works
		// alongside `--print …`. Anything past that is help-text prose.
		for i := 0; i < len(fields) && i < 2; i++ {
			tok := strings.TrimRight(fields[i], ",")
			if !strings.HasPrefix(tok, "--") {
				continue
			}
			for _, name := range strings.Split(tok, "/") {
				name = strings.TrimSpace(name)
				name = strings.TrimRight(name, ",")
				if !strings.HasPrefix(name, "--") || len(name) <= 2 {
					continue
				}
				flags[name] = true
			}
		}
	}
	return flags
}

// normalizeMode maps user-friendly aliases to Kimi CLI mode values.
func normalizeMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "yolo", "force", "bypass", "auto":
		return "yolo"
	case "plan":
		return "plan"
	case "quiet":
		return "quiet"
	default:
		return "default"
	}
}

// promptFromInput serializes a canonical Input turn into the single prompt
// passed to `kimi --prompt`. On resume kimi owns the conversation history
// natively, so only the last user message is sent; on a fresh turn the
// role-labeled messages are sent.
func promptFromInput(in runtime.Input, isResume bool) string {
	var userParts []string
	var otherParts []string
	for _, m := range in.Messages {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		switch m.Role {
		case "user":
			userParts = append(userParts, content)
		case "system":
			otherParts = append(otherParts, "System instructions:\n"+content)
		case "assistant":
			otherParts = append(otherParts, "Assistant:\n"+content)
		case "tool":
			otherParts = append(otherParts, "Tool result:\n"+content)
		default:
			otherParts = append(otherParts, content)
		}
	}
	if isResume {
		if len(userParts) == 0 {
			return ""
		}
		return userParts[len(userParts)-1]
	}

	if len(otherParts) == 0 {
		return strings.Join(userParts, "\n\n")
	}
	all := append([]string(nil), otherParts...)
	for _, u := range userParts {
		all = append(all, "User:\n"+u)
	}
	return strings.Join(all, "\n\n")
}

// extractResumeSessionID pulls the native Kimi session id out of the "To
// resume this session: kimi -r <uuid>" trailer line.
func extractResumeSessionID(line string) string {
	// Format: "To resume this session: kimi -r <uuid>"
	parts := strings.Fields(line)
	for i, p := range parts {
		if p == "-r" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// toolCallArgs turns a raw Kimi tool-call arguments string (JSON) into
// canonical ToolCall.Arguments. A parseable JSON object is carried as-is;
// anything else is kept verbatim under "arguments".
func toolCallArgs(input string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(input), &m); err == nil && m != nil {
		return m
	}
	return map[string]any{"arguments": input}
}

// mergeEnv returns base env with entries from extra overriding same-key
// entries. This prevents duplicate keys (e.g. two PATH entries) which would
// make the override silently ignored on Linux (getenv returns the first match).
func mergeEnv(base, extra []string) []string {
	keys := make(map[string]bool, len(extra))
	for _, e := range extra {
		if k, _, ok := strings.Cut(e, "="); ok {
			keys[k] = true
		}
	}
	merged := make([]string, 0, len(base)+len(extra))
	for _, e := range base {
		if k, _, ok := strings.Cut(e, "="); ok && keys[k] {
			continue
		}
		merged = append(merged, e)
	}
	return append(merged, extra...)
}

// redactArgs returns a copy of args with values after sensitive flag names
// masked, so secrets never land in logs.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)

	sensitiveFlags := []string{
		"--api-key", "--api_key", "--apikey",
		"--token", "--secret", "--password",
		"-k",
	}

	for i := 0; i < len(out); i++ {
		arg := strings.ToLower(out[i])
		for _, f := range sensitiveFlags {
			if strings.HasPrefix(arg, f+"=") {
				out[i] = out[i][:strings.Index(out[i], "=")+1] + "***"
				break
			}
			if arg == f && i+1 < len(out) {
				out[i+1] = "***"
				i++
				break
			}
		}
	}
	return out
}

// truncate limits a string to maxRunes runes, appending an ellipsis.
func truncate(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "..."
}

// splitCommand splits "bin arg1 arg2" into the binary and its extra args.
func splitCommand(cmd string) (bin string, args []string) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return "kimi", nil
	}
	return parts[0], parts[1:]
}

// envOrDefault returns the value of env key or fallback when empty.
func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// splitEnv splits a "K=V,K2=V2" (or "K=V K2=V2") string into KEY=VALUE pairs.
func splitEnv(raw string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		part = strings.TrimSpace(part)
		if part != "" && strings.Contains(part, "=") {
			out = append(out, part)
		}
	}
	return out
}
