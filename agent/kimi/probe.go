package kimi

import (
	"context"
	"io"
	"strings"
	"time"

	agentprocess "github.com/tianlinzz/agent-cli-gateway/agent/process"
)

func ParseHelpFlags(help string) FlagSupport {
	for _, field := range strings.Fields(help) {
		if strings.Trim(field, "[](),") == "--print" {
			return FlagSupport{Print: true}
		}
	}
	return FlagSupport{}
}

func ProbeFlags(parent context.Context, command []string, timeout time.Duration) FlagSupport {
	if len(command) == 0 {
		return FlagSupport{}
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	probe := append(append([]string(nil), command...), "--help")
	proc, err := agentprocess.Start(ctx, agentprocess.Spec{Command: probe})
	if err != nil {
		return FlagSupport{}
	}
	output, readErr := io.ReadAll(proc.Stdout())
	waitErr := proc.Wait()
	if readErr != nil || waitErr != nil {
		return FlagSupport{}
	}
	return ParseHelpFlags(string(output) + "\n" + proc.StderrString())
}
