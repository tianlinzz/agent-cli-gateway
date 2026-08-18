package integration

import (
	"os"
	"strings"
	"testing"
)

func readDockerfile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// The base image must be a pure environment: runtimes, the agent CLIs, and
// nsjail — but never a startup command. Startup belongs exclusively to the
// product image, so derived images always control how they run.
func TestBaseImagePreparesEnvironmentWithoutStartupCommand(t *testing.T) {
	base := readDockerfile(t, "../docker/base/Dockerfile")
	for _, tool := range []string{"nginx", "python3", "tini", "git", "curl"} {
		if !strings.Contains(base, "        "+tool+" ") {
			t.Fatalf("base image must install %s", tool)
		}
	}
	if !strings.Contains(base, "COPY --from=go-toolchain /usr/local/go") {
		t.Fatal("base image must ship the Go toolchain")
	}
	if !strings.Contains(base, "COPY --from=node-runtime /usr/local/bin/node") {
		t.Fatal("base image must ship the Node.js runtime")
	}
	if !strings.Contains(base, "COPY --from=nsjail-builder /nsjail/nsjail") {
		t.Fatal("base image must compile and ship nsjail")
	}
	if !strings.Contains(base, "python-is-python3") {
		t.Fatal("base image must provide the python command via python-is-python3")
	}
	for _, cli := range []string{"@openai/codex", "@anthropic-ai/claude-code", "@moonshot-ai/kimi-code"} {
		if !strings.Contains(base, cli) {
			t.Fatalf("base image must ship the %s CLI", cli)
		}
	}
	for _, directive := range []string{"ENTRYPOINT", "CMD", "EXPOSE", "HEALTHCHECK"} {
		for _, line := range strings.Split(base, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "#") && strings.HasPrefix(trimmed, directive) {
				t.Fatalf("base image must not declare %s (startup belongs to the product image): %q", directive, trimmed)
			}
		}
	}
}

func TestProductImageUsesTiniAsContainerInit(t *testing.T) {
	product := readDockerfile(t, "../Dockerfile")
	if !strings.Contains(product, "FROM ${BASE_IMAGE}") {
		t.Fatal("product image must build on the base image")
	}
	if !strings.Contains(product, `ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]`) {
		t.Fatal("product image must run tini as PID 1 and launch the configuration entrypoint")
	}
	if !strings.Contains(product, `CMD ["gateway"]`) {
		t.Fatal("product image must default to running the gateway")
	}
}

func TestContainerEntrypointGeneratesCallerAuth(t *testing.T) {
	data, err := os.ReadFile("../docker/entrypoint.sh")
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}
	script := string(data)
	if !strings.Contains(script, "callers = [{ id = \"container\", tokens = [") {
		t.Fatal("entrypoint must generate the configured caller-token schema")
	}
	if strings.Contains(script, `printf 'token =`) {
		t.Fatal("entrypoint must not generate removed auth.token")
	}
}
