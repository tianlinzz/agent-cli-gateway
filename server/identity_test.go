package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithUserIdentity_StrictMode_MissingHeader_401(t *testing.T) {
	mw := withUserIdentity("X-User-Id", "strict")
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/session", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("strict missing header: got %d, want 401", rec.Code)
	}
}

func TestWithUserIdentity_StrictMode_ValidHeader_InjectsCtx(t *testing.T) {
	mw := withUserIdentity("X-User-Id", "strict")
	var got string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = UserIDFromContext(r.Context())
		w.WriteHeader(200)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/session", nil)
	req.Header.Set("X-User-Id", "alice")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("valid header: got %d, want 200", rec.Code)
	}
	if got != "alice" {
		t.Errorf("injected identity = %q, want %q", got, "alice")
	}
}

func TestWithUserIdentity_StrictMode_BadFormat_400(t *testing.T) {
	mw := withUserIdentity("X-User-Id", "strict")
	cases := []string{"alice@example.com", "../etc", "a b", string(make([]byte, 65))}
	for _, bad := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/session", nil)
		req.Header.Set("X-User-Id", bad)
		mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler should not be called")
		})).ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad format %q: got %d, want 400", bad, rec.Code)
		}
	}
}

func TestWithUserIdentity_AnonymousMode_MissingHeader_DefaultsAnonymous(t *testing.T) {
	mw := withUserIdentity("X-User-Id", "anonymous")
	var got string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = UserIDFromContext(r.Context())
		w.WriteHeader(200)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/session", nil)
	h.ServeHTTP(rec, req)
	if got != "anonymous" {
		t.Errorf("anonymous mode missing header: identity = %q, want %q", got, "anonymous")
	}
}

func TestUserIDFromContext_Empty(t *testing.T) {
	if got := UserIDFromContext(context.Background()); got != "" {
		t.Errorf("empty ctx: got %q, want empty", got)
	}
}
