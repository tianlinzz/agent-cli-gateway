package process

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStartUsesDirectoryEnvironmentAndPipes(t *testing.T) {
	t.Setenv("GO_WANT_AGENT_PROCESS_HELPER", "1")
	dir := t.TempDir()
	p, err := Start(context.Background(), Spec{
		Command: helperCommand("exchange"),
		Dir:     dir,
		Env:     []string{"AGENT_PROCESS_TEST_VALUE=replaced"},
		Stdin:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(p.Stdin(), "request\n"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if err := p.CloseStdin(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	out, err := io.ReadAll(p.Stdout())
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("wait: %v; stderr=%s", err, p.StderrString())
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	want := strings.Join([]string{realDir, "replaced", "request"}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
}

func TestStartRejectsEmptyCommand(t *testing.T) {
	_, err := Start(context.Background(), Spec{})
	if err == nil || !strings.Contains(err.Error(), "empty command") {
		t.Fatalf("error = %v, want empty command", err)
	}
}

func TestWaitIsIdempotentAndConcurrentSafe(t *testing.T) {
	t.Setenv("GO_WANT_AGENT_PROCESS_HELPER", "1")
	p, err := Start(context.Background(), Spec{Command: helperCommand("success")})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- p.Wait()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
	}
}

func TestContextCancellationReapsProcess(t *testing.T) {
	t.Setenv("GO_WANT_AGENT_PROCESS_HELPER", "1")
	ctx, cancel := context.WithCancel(context.Background())
	p, err := Start(ctx, Spec{Command: helperCommand("block")})
	if err != nil {
		t.Fatal(err)
	}
	cancel()

	if err := p.Wait(); err == nil {
		t.Fatal("wait error = nil, want cancellation-driven process failure")
	}
	if processExists(p.PID()) {
		t.Fatalf("process %d still exists after Wait", p.PID())
	}
}

func TestMergeEnvReplacesCaseInsensitivelyAndAppends(t *testing.T) {
	got := MergeEnv(
		[]string{"PATH=/base", "KEEP=yes", "INVALID"},
		[]string{"path=/override", "NEW=value", "INVALID"},
	)
	if strings.Join(got, "\n") != strings.Join([]string{"path=/override", "KEEP=yes", "INVALID", "NEW=value"}, "\n") {
		t.Fatalf("MergeEnv() = %#v", got)
	}
}

func helperCommand(mode string) []string {
	return []string{os.Args[0], "-test.run=TestAgentProcessHelper", "--", mode}
}

func TestAgentProcessHelper(t *testing.T) {
	if os.Getenv("GO_WANT_AGENT_PROCESS_HELPER") != "1" {
		return
	}
	args := os.Args
	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(args) {
		fmt.Fprintln(os.Stderr, "missing helper mode")
		os.Exit(2)
	}
	switch args[separator+1] {
	case "success":
		os.Exit(0)
	case "exchange":
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(filepath.Clean(cwd))
		fmt.Println(os.Getenv("AGENT_PROCESS_TEST_VALUE"))
		fmt.Print(string(input))
		os.Exit(0)
	case "block":
		for {
			time.Sleep(time.Second)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
}
