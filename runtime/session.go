package runtime

import "time"

// SessionStatus is the lifecycle state of a gateway session. The status enum
// doubles as the single-active-turn flag: SessionTurnActive means a turn is in
// flight and BeginTurn on such a session fails.
type SessionStatus string

const (
	// SessionActive means the session exists and is ready for a turn. This is
	// the status Create and EndTurn land on.
	SessionActive SessionStatus = "active"
	// SessionTurnActive means a turn is currently in flight. Only one active
	// turn per session is allowed at any time.
	SessionTurnActive SessionStatus = "turn_active"
	// SessionClosing means teardown has started; new turns are rejected.
	SessionClosing SessionStatus = "closing"
	// SessionClosed is the terminal status; all operations are rejected.
	SessionClosed SessionStatus = "closed"
)

// Valid reports whether s is a known SessionStatus.
func (s SessionStatus) Valid() bool {
	switch s {
	case SessionActive, SessionTurnActive, SessionClosing, SessionClosed:
		return true
	}
	return false
}

// SessionRecord is the immutable-by-contract metadata of one gateway session.
// It is pure data: only strings, a time.Time, and a status enum. No maps,
// slices, or pointers — a value copy is a deep copy, so the store can hand out
// records without leaking internal state. Workers bind the workspace directory
// by WorkspaceID (via the workspace.Resolver), and the API layer scopes every
// access by CallerID.
type SessionRecord struct {
	// ID is the gateway-assigned session identifier (StartRequest.SessionID).
	ID string
	// ModelID is the public model/agent identifier (StartRequest.ModelID).
	ModelID string
	// NativeSessionID is the agent-native session ID used to resume a
	// recovered persistent adapter session.
	NativeSessionID string
	// CallerID is the tenant/user that owns the session. All access is scoped
	// to it.
	CallerID string
	// WorkspaceID is the opaque client-supplied workspace identifier the
	// worker resolves to a controlled directory.
	WorkspaceID string
	// WorkerID and NodeID identify the execution that serves this session
	// (reported by the worker in the Health handshake; node = os.Hostname()
	// in the single-node deployment). The API layer fills them best-effort
	// when the execution handle exposes its worker identity; they remain
	// empty for backends that do not.
	WorkerID string
	NodeID   string

	// Status is the session lifecycle state.
	Status SessionStatus

	// CreatedAt is the creation time; it is immutable after Create.
	CreatedAt time.Time
	// UpdatedAt is bumped on every mutation.
	UpdatedAt time.Time
	// ExpiresAt is the expiry deadline; zero means "no expiry". Expired
	// sessions are purged lazily on Get and physically by Prune.
	ExpiresAt time.Time
}
