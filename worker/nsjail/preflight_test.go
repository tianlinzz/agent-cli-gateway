package nsjail

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/config"
)

func TestPreflight_DisabledWhenNotRequired(t *testing.T) {
	iso := config.DefaultGatewayRuntimeConfig().Isolation
	iso.Required = false // test profile only
	if err := Preflight(context.Background(), iso); err != nil {
		t.Fatalf("Preflight must pass when isolation is not required: %v", err)
	}
}

func TestPreflight_FailClosedBinaryChecks(t *testing.T) {
	dir := t.TempDir()
	nonExe := filepath.Join(dir, "nsjail-noexec")
	if err := os.WriteFile(nonExe, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
	}{
		{"empty binary path", ""},
		{"missing binary", filepath.Join(dir, "missing")},
		{"non-executable binary", nonExe},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iso := config.DefaultGatewayRuntimeConfig().Isolation
			iso.Required = true
			iso.BinaryPath = tt.path
			err := Preflight(context.Background(), iso)
			if err == nil {
				t.Fatal("Preflight must fail closed")
			}
			if !strings.Contains(err.Error(), "nsjail") {
				t.Errorf("error should mention nsjail: %v", err)
			}
		})
	}
}

// TestPreflight_ExecutableBinaryOnNonLinux exercises the platform boundary:
// on darwin (and other non-linux hosts) a real nsjail cannot run, so the
// preflight must fail closed rather than claim the sandbox is available. On
// linux the real version/capability checks run and require an actual nsjail
// install, which unit tests must not depend on — that smoke validation is
// deferred to Linux CI (task 7).
func TestPreflight_ExecutableBinaryOnNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("real nsjail smoke validation is deferred to Linux CI (task 7)")
	}
	exe := filepath.Join(t.TempDir(), "nsjail")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	iso := config.DefaultGatewayRuntimeConfig().Isolation
	iso.Required = true
	iso.BinaryPath = exe
	err := Preflight(context.Background(), iso)
	if err == nil {
		t.Fatal("preflight must fail closed on a non-linux host even with an executable binary")
	}
	if !strings.Contains(err.Error(), "linux") {
		t.Errorf("error should explain the linux requirement: %v", err)
	}
}
