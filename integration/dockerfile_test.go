package integration

import (
	"os"
	"strings"
	"testing"
)

func TestDockerfileUsesTiniAsContainerInit(t *testing.T) {
	data, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(data)
	if !strings.Contains(dockerfile, "        tini ") {
		t.Fatal("runtime image must install tini")
	}
	if !strings.Contains(dockerfile, `ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]`) {
		t.Fatal("tini must be PID 1 and launch the configuration entrypoint")
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
