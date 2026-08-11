package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDevAgentEnabledDetectsInstalledCommandsAndHonorsOverride(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"claude", "codex"} {
		path := filepath.Join(bin, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	output := runDevAgentHelper(t, bin, `
printf '%s\n' "$(dev_agent_enabled auto claude)"
printf '%s\n' "$(dev_agent_enabled auto codex)"
printf '%s\n' "$(dev_agent_enabled auto kimi)"
printf '%s\n' "$(dev_agent_enabled on missing-kimi)"
printf '%s\n' "$(dev_agent_enabled off claude)"
`)
	want := []string{"true", "true", "false", "true", "false"}
	got := strings.Fields(output)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("enabled results = %#v, want %#v", got, want)
	}
}

func runDevAgentHelper(t *testing.T, path, body string) string {
	t.Helper()
	command := exec.Command("bash", "-c", `source ./scripts/dev-agents.sh`+body)
	command.Dir = ".."
	command.Env = []string{"PATH=" + path + ":/usr/bin:/bin"}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run helper: %v: %s", err, output)
	}
	return string(output)
}
