package openai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// idempotencyEndpoint is the only surface that honors Idempotency-Key today.
const idempotencyEndpoint = "/v1/chat/completions"

// maxIdempotencyKeyBytes bounds the client-supplied Idempotency-Key header.
const maxIdempotencyKeyBytes = 255

// idemState tracks one request's in-flight idempotency claim. It is owned by
// the single request goroutine; no locking is needed.
type idemState struct {
	caller   string
	endpoint string
	key      string
	runID    string
	// delivered reports whether the turn may have reached an execution. Once
	// true the key must NEVER be released for reuse: a failure after delivery
	// has ambiguous side effects, so the record is completed without a body
	// (a duplicate gets a 409 status reference, never an implicit re-run).
	delivered bool
	settled   bool
}

// idemConflictResponse is the 409 body for idempotency conflicts: the OpenAI
// error envelope plus a reference to the run that owns the key.
type idemConflictResponse struct {
	Error  apiError `json:"error"`
	RunID  string   `json:"run_id,omitempty"`
	Status string   `json:"status,omitempty"`
}

// idempotencyFingerprint digests the semantically relevant request payload
// into a stable hex fingerprint: the model, the gateway session and workspace
// ids exactly as the client supplied them (before the gateway generates a
// session id), and the normalized messages/tools. encoding/json sorts map
// keys, so the encoding of these normalized value types is deterministic.
// The stream flag and opaque metadata are deliberately excluded: the key
// binds the turn's intent, not its presentation.
func idempotencyFingerprint(req ChatCompletionRequest, sessionID, workspaceID string, input runtime.Input) string {
	canonical := struct {
		Model       string            `json:"model"`
		SessionID   string            `json:"session_id"`
		WorkspaceID string            `json:"workspace_id"`
		Messages    []runtime.Message `json:"messages"`
		Tools       []runtime.Tool    `json:"tools"`
	}{
		Model:       req.Model,
		SessionID:   sessionID,
		WorkspaceID: workspaceID,
		Messages:    input.Messages,
		Tools:       input.Tools,
	}
	// Marshal cannot fail: the payload is strings, slices, and maps with
	// string keys decoded from JSON. Treat the impossible failure as
	// "no fingerprint" so the request falls back to non-idempotent behavior
	// rather than colliding on an empty digest.
	data, err := json.Marshal(canonical)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// beginIdempotency claims the Idempotency-Key for this request BEFORE session
// resolution and admission: preventing duplicate admission is the whole point
// of the key, so the check must run before any turn state is built. It returns
// (nil, false) when the feature is disabled or no key is supplied — the
// request then behaves exactly as without the feature. A true second return
// means the response was already written (duplicate, conflict, or error) and
// the caller must return immediately.
func (h *Handler) beginIdempotency(w http.ResponseWriter, r *http.Request, callerID string, req ChatCompletionRequest, sessionID, workspaceID, runID string, input runtime.Input) (*idemState, bool) {
	if h.idempotency == nil {
		return nil, false
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		return nil, false
	}
	if len(key) > maxIdempotencyKeyBytes {
		writeError(w, http.StatusBadRequest, invalidRequest("Idempotency-Key exceeds 255 bytes"))
		return nil, true
	}
	fingerprint := idempotencyFingerprint(req, sessionID, workspaceID, input)
	if fingerprint == "" {
		// Defensive: the fingerprint payload cannot fail to encode. Never
		// block the request on it; log and proceed without dedup.
		h.logger.Warn("openai: idempotency fingerprint failed", "run_id", runID)
		return nil, false
	}
	rec, conflict, err := h.idempotency.Begin(r.Context(), runtime.IdempotencyRecord{
		Key:         key,
		CallerID:    callerID,
		Endpoint:    idempotencyEndpoint,
		Fingerprint: fingerprint,
		RunID:       runID,
		Status:      runtime.IdempotencyActive,
		ExpiresAt:   h.now().Add(h.idemTTL),
	})
	switch {
	case errors.Is(err, runtime.ErrIdempotencyMismatch):
		writeJSON(w, http.StatusConflict, idemConflictResponse{
			Error: apiErr("Idempotency-Key was already used with a different request payload", errorTypeInvalid, "idempotency_conflict"),
		})
		return nil, true
	case err != nil:
		writeError(w, http.StatusInternalServerError, serverError("failed to claim idempotency key"))
		return nil, true
	}
	if conflict {
		h.writeIdemDuplicate(w, r, req, rec)
		return nil, true
	}
	return &idemState{caller: callerID, endpoint: idempotencyEndpoint, key: key, runID: runID}, false
}

// writeIdemDuplicate answers a request whose key is already claimed by an
// identical request:
//   - active: 409 in_progress with the owning run id — a second turn is never
//     started;
//   - completed with a stored body (non-streaming only): the exact recorded
//     response bytes are replayed;
//   - completed without a body (streaming responses are not replayable;
//     failed/cancelled/outcome_unknown turns are never implicitly re-run):
//     409 with a run status reference.
func (h *Handler) writeIdemDuplicate(w http.ResponseWriter, r *http.Request, req ChatCompletionRequest, rec runtime.IdempotencyRecord) {
	w.Header().Set(h.runHeader, rec.RunID)
	h.setIdemSessionHeader(w, r, rec.RunID)
	if rec.Status == runtime.IdempotencyActive {
		writeJSON(w, http.StatusConflict, idemConflictResponse{
			Error:  apiErr("an identical request with this Idempotency-Key is already in progress", errorTypeInvalid, "idempotency_in_progress"),
			RunID:  rec.RunID,
			Status: "in_progress",
		})
		return
	}
	if !req.Stream && len(rec.ResponseBody) > 0 {
		// Byte-exact replay of the recorded non-streaming response.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Idempotency-Replayed", "true")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rec.ResponseBody)
		return
	}
	status := string(rec.RunStatus)
	if status == "" {
		status = "completed"
	}
	msg := "the original request completed with a non-replayable response (streaming responses are never stored)"
	if rec.RunStatus == runtime.RunOutcomeUnknown {
		// The completion outcome of the original turn could not be confirmed.
		// The gateway NEVER implicitly re-runs it; the caller must inspect the
		// referenced run and retry with a NEW key if it decides to.
		msg = "the original request's outcome is unknown and is never implicitly re-run"
	}
	writeJSON(w, http.StatusConflict, idemConflictResponse{
		Error:  apiErr(msg, errorTypeInvalid, "idempotency_not_replayable"),
		RunID:  rec.RunID,
		Status: status,
	})
}

// setIdemSessionHeader best-effort echoes the original turn's gateway session
// id on a duplicate/replay response by joining the referenced run record, so a
// client that retried its very first request (no session id supplied) still
// learns the session the turn ran on. A pruned run record simply skips the
// header.
func (h *Handler) setIdemSessionHeader(w http.ResponseWriter, r *http.Request, runID string) {
	if runID == "" {
		return
	}
	run, err := h.runs.Get(r.Context(), runID)
	if err != nil || run.SessionID == "" {
		return
	}
	w.Header().Set(h.sessionHeader, run.SessionID)
}

// completeIdem records the terminal outcome of the claimed key. body is the
// exact response bytes, saved only for a replayable non-streaming success;
// every other outcome completes the record WITHOUT a body so a duplicate gets
// a 409 status reference instead of a replay. It is nil-safe and idempotent.
func (h *Handler) completeIdem(st *idemState, status runtime.RunStatus, body []byte) {
	if st == nil || st.settled {
		return
	}
	st.settled = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.idempotency.Complete(ctx, st.caller, st.endpoint, st.key, st.runID, status, body); err != nil {
		h.logger.Warn("openai: idempotency complete", "key", st.key, "run_id", st.runID, "error", err)
	}
}

// settleIdempotency is the deferred safety net for every path that returns
// before completeIdem ran. It guarantees a claimed key can never wedge in the
// active state forever:
//   - if the turn may have reached an execution (delivered), the record is
//     completed WITHOUT a body and with an outcome_unknown reference — the
//     side effects are ambiguous, so the key must never be silently reusable;
//   - if the request was rejected before any execution was admitted
//     (validation, admission, turn arbitration, run-record creation), the key
//     is released (deleted) so the caller may retry the identical request.
func (h *Handler) settleIdempotency(st *idemState) {
	if st == nil || st.settled {
		return
	}
	st.settled = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if st.delivered {
		if err := h.idempotency.Complete(ctx, st.caller, st.endpoint, st.key, st.runID, runtime.RunOutcomeUnknown, nil); err != nil {
			h.logger.Warn("openai: idempotency settle after delivery", "key", st.key, "run_id", st.runID, "error", err)
		}
		return
	}
	if err := h.idempotency.Delete(ctx, st.caller, st.endpoint, st.key); err != nil && !errors.Is(err, runtime.ErrIdempotencyNotFound) {
		h.logger.Warn("openai: idempotency release", "key", st.key, "run_id", st.runID, "error", err)
	}
}
