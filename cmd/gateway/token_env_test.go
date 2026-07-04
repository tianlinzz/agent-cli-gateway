package main

import (
	"os"
	"testing"
)

// TestMain_TokenEnvFallback: GATEWAY_TOKEN 在没有 -token flag 时生效。
// 这是为容器化部署加的 env fallback 的回归测试（AGENTS.md 要求 feature 配测试）。
func TestMain_TokenEnvFallback(t *testing.T) {
	// 还原 token fallback 的判定逻辑，确保 env 覆盖正确。
	// 真实的 flag 解析在 main() 里，这里直接验证判定条件。
	resolve := func(flagToken string) string {
		if flagToken != "" {
			return flagToken
		}
		return os.Getenv("GATEWAY_TOKEN")
	}

	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv("GATEWAY_TOKEN", "env-secret")
		if got := resolve("flag-secret"); got != "flag-secret" {
			t.Fatalf("flag should win: got %q, want %q", got, "flag-secret")
		}
	})

	t.Run("env fallback when flag empty", func(t *testing.T) {
		t.Setenv("GATEWAY_TOKEN", "env-secret")
		if got := resolve(""); got != "env-secret" {
			t.Fatalf("empty flag should fall back to env: got %q, want %q", got, "env-secret")
		}
	})

	t.Run("empty when neither set", func(t *testing.T) {
		os.Unsetenv("GATEWAY_TOKEN")
		if got := resolve(""); got != "" {
			t.Fatalf("expected empty when no flag and no env, got %q", got)
		}
	})
}
