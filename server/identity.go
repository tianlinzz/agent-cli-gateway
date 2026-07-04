package server

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
)

// identityKey is the context key for the caller identity.
type identityKey struct{}

// UserIDFromContext returns the caller identity injected by withUserIdentity,
// or "" if not present.
func UserIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(identityKey{}).(string)
	return v
}

// withOwnerContext is a test helper that injects an identity into context.
// Production code gets identity via the withUserIdentity middleware.
func withOwnerContext(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, identityKey{}, userID)
}

// userIDPattern restricts identity strings to safe characters. It doubles as
// a path-injection guard (identity is used as a logging/grouping key; while
// it does NOT enter workDir paths in the current design, keeping it safe
// avoids future foot-guns).
var userIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// withUserIdentity returns middleware that resolves the caller identity from
// the configured header and injects it into the request context.
//
// mode == "strict":   missing/invalid header → 401/400.
// mode == "anonymous": missing header → identity "anonymous"; present header
//                     still validated and used as given.
func withUserIdentity(header, mode string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid := r.Header.Get(header)
			if uid == "" {
				if mode == "anonymous" {
					uid = "anonymous"
				} else {
					writeError(w, http.StatusUnauthorized, "missing identity header: "+header)
					return
				}
			} else if !userIDPattern.MatchString(uid) {
				writeError(w, http.StatusBadRequest, "invalid identity format")
				return
			}
			slog.Debug("caller identity", "user", uid, "path", r.URL.Path)
			r = r.WithContext(context.WithValue(r.Context(), identityKey{}, uid))
			next.ServeHTTP(w, r)
		})
	}
}
