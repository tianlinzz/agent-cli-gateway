package worker

// ProtocolVersion is the single source of truth for the supervisor <-> worker
// handshake protocol version. Both binaries (cmd/gateway's supervisor and
// cmd/gateway-worker) import this package, so gateway and worker always speak
// the constant from the same build. The worker reports it in the Health
// handshake (HealthResponse.protocol_version); the supervisor compares it
// against its own and rejects the session (fail-closed) on any mismatch — a
// mixed-version pair must fail loudly at start, never drift mid-session.
//
// Bump it on any incompatible change to the handshake semantics (field
// meaning, negotiation rules). Merely additive proto fields do not require a
// bump: proto3 unknown-field tolerance keeps older peers working.
const ProtocolVersion = 1
