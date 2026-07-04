package config

import "time"

// GatewayConfig holds agent-cli-gateway gateway-specific configuration.
type GatewayConfig struct {
	Port        int      `toml:"port" json:"port"`
	Token       string   `toml:"token" json:"token"`
	CORSOrigins []string `toml:"cors_origins" json:"cors_origins"`
	// DataDir is used for agent session transcripts and temp files.
	DataDir string `toml:"data_dir" json:"data_dir"`

	// --- Multi-user session governance (P0) ---

	// UserIDHeader is the HTTP header name carrying the caller identity.
	// Default "X-User-Id". Caller-agnostic: any upstream can set its own header.
	UserIDHeader string `toml:"user_id_header" json:"user_id_header"`
	// IdentityMode is "strict" (missing header → 401) or "anonymous"
	// (missing header → treated as user "anonymous"). Default "strict".
	IdentityMode string `toml:"identity_mode" json:"identity_mode"`
	// MaxSessionsPerUser caps concurrent live sessions per caller.
	// 0 = unlimited (disables per-caller LRU). Default 5.
	MaxSessionsPerUser int `toml:"max_sessions_per_user" json:"max_sessions_per_user"`
	// SessionIdleTTL is how long a session with no in-flight turn is kept
	// before the reaper evicts it (kill + remove). 0 = unlimited.
	// Default 2h.
	SessionIdleTTL time.Duration `toml:"session_idle_ttl" json:"session_idle_ttl"`
}

// DefaultGatewayConfig returns sensible defaults.
func DefaultGatewayConfig() GatewayConfig {
	return GatewayConfig{
		Port:               4096,
		DataDir:            "",
		UserIDHeader:       "X-User-Id",
		IdentityMode:       "strict",
		MaxSessionsPerUser: 5,
		SessionIdleTTL:     2 * time.Hour,
	}
}
