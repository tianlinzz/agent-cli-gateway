package config

import (
	"testing"
	"time"
)

func TestDefaultGatewayConfig_HasNewGovernanceFields(t *testing.T) {
	c := DefaultGatewayConfig()
	if c.UserIDHeader != "X-User-Id" {
		t.Errorf("UserIDHeader default = %q, want %q", c.UserIDHeader, "X-User-Id")
	}
	if c.IdentityMode != "strict" {
		t.Errorf("IdentityMode default = %q, want %q", c.IdentityMode, "strict")
	}
	if c.MaxSessionsPerUser != 5 {
		t.Errorf("MaxSessionsPerUser default = %d, want 5", c.MaxSessionsPerUser)
	}
	if c.SessionIdleTTL != 2*time.Hour {
		t.Errorf("SessionIdleTTL default = %v, want 2h", c.SessionIdleTTL)
	}
}
