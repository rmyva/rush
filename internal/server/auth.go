package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const cookieName = "rush_session"

const maxAuthBodyBytes = 4 << 10

// Auth handles token-based authentication for the web UI.
// The token is generated at startup, printed to the terminal, and must be
// supplied by the browser user before a session cookie is issued.
type Auth struct {
	token string // hex-encoded random token
}

func newAuth() *Auth {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic("server: cannot generate auth token: " + err.Error())
	}
	return &Auth{token: hex.EncodeToString(b)}
}

// Token returns the human-visible token string to print in the terminal.
func (a *Auth) Token() string { return a.token }

// secureCompare reports whether got equals want using a constant-time
// comparison, so a timing side-channel can't be used to guess the auth token
// one character at a time. subtle.ConstantTimeCompare requires equal-length
// slices (it returns 0, i.e. unequal, for mismatched lengths without a timing
// leak of its own — Go's runtime doesn't short-circuit on len() checks), so a
// length mismatch is treated as "not equal" up front without calling into it.
func secureCompare(got, want string) bool {
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// HandleAuth handles POST /auth — validates the submitted token and sets a
// session cookie on success.
func (a *Auth) HandleAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBodyBytes)
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&body)
	if err == nil {
		// Require exactly one JSON value while retaining legal trailing
		// whitespace. Decode again so MaxBytesReader bounds trailing data too.
		var extra any
		err = decoder.Decode(&extra)
		if errors.Is(err, io.EOF) {
			err = nil
		} else if err == nil {
			err = errors.New("multiple JSON values")
		}
	}
	if err != nil || body.Token == "" {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !secureCompare(body.Token, a.token) {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    a.token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Now().Add(30 * 24 * time.Hour),
	})
	w.WriteHeader(http.StatusOK)
}

// HandleAuthCheck handles GET /auth/check — returns 200 if authenticated,
// 401 otherwise. Used by the React app to skip the login page on reload.
func (a *Auth) HandleAuthCheck(w http.ResponseWriter, r *http.Request) {
	if a.isValid(r) {
		w.WriteHeader(http.StatusOK)
	} else {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

// Middleware wraps a handler, requiring a valid session cookie OR a valid
// token in the Authorization header ("Bearer <token>") or query param "token".
// This covers WebSocket upgrades where browsers cannot set custom headers.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.isValid(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isValid accepts any of:
//   - session cookie set after successful /auth POST
//   - Authorization: Bearer <token> header
//   - ?token=<token> query parameter (for WebSocket clients)
func (a *Auth) isValid(r *http.Request) bool {
	// Cookie (primary method for browser clients).
	if c, err := r.Cookie(cookieName); err == nil && secureCompare(c.Value, a.token) {
		return true
	}
	// Authorization header (Bearer token).
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") &&
		secureCompare(strings.TrimPrefix(auth, "Bearer "), a.token) {
		return true
	}
	// Query parameter (fallback for WS where headers are hard to set).
	if secureCompare(r.URL.Query().Get("token"), a.token) {
		return true
	}
	return false
}
