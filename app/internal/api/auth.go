package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"mailroom/internal/core"
)

// Scope is an API permission level (G-4).
type Scope string

// Scopes. Admin may do everything; read may only call read operations
// (GET by default).
const (
	ScopeRead  Scope = "read"
	ScopeAdmin Scope = "admin"
)

// Valid reports whether s is a defined scope.
func (s Scope) Valid() bool { return s == ScopeRead || s == ScopeAdmin }

// Allows reports whether credentials with scope s may call an operation
// that requires scope required.
func (s Scope) Allows(required Scope) bool {
	switch s {
	case ScopeAdmin:
		return true
	case ScopeRead:
		return required == ScopeRead
	}
	return false
}

// Credential names shared by the API middleware and the auth package.
const (
	// TokenPrefix starts every API token ("mr_...").
	TokenPrefix = "mr_"
	// SessionCookie is the UI session cookie.
	SessionCookie = "mailroom_session"
	// CSRFHeader must carry the session's CSRF token on every non-GET
	// request authenticated by the session cookie.
	CSRFHeader = "X-CSRF-Token"
)

// Principal is the authenticated caller of a request.
type Principal struct {
	Actor core.Actor
	Scope Scope
}

// Authenticator validates credentials. It is implemented by the auth
// package and injected in Config, so api does not import auth.
type Authenticator interface {
	// AuthenticateToken validates an "mr_..." API token. Unknown, revoked or
	// expired tokens return an error wrapping ErrUnauthorized.
	AuthenticateToken(ctx context.Context, token string) (Principal, error)
	// AuthenticateSession validates a session cookie value and returns the
	// session's CSRF token. Unknown or expired sessions return an error
	// wrapping ErrUnauthorized.
	AuthenticateSession(ctx context.Context, cookie string) (p Principal, csrfToken string, err error)
}

type principalKey struct{}

// WithPrincipal returns a copy of ctx carrying p (and p.Actor as the
// core actor, for audit).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(core.WithActor(ctx, p.Actor), principalKey{}, p)
}

// PrincipalFromContext returns the request's authenticated caller.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Credential errors reported by credentials.
var (
	// errCSRF: a session request without a valid CSRF token.
	errCSRF = errors.New("missing or invalid " + CSRFHeader + " header")
	// errMalformedAuth: an Authorization header that isn't "Bearer mr_...".
	errMalformedAuth = fmt.Errorf(`expected "Authorization: Bearer mr_...": %w`, ErrUnauthorized)
)

// safeMethod reports whether m never changes state (no CSRF check needed).
func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// credentials authenticates r. ok is false when r carries no credentials.
// The Authorization header wins over the session cookie.
func (a *API) credentials(r *http.Request) (p Principal, ok bool, err error) {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, tok, found := strings.Cut(h, " ")
		tok = strings.TrimSpace(tok)
		if !found || !strings.EqualFold(scheme, "Bearer") || !strings.HasPrefix(tok, TokenPrefix) {
			return Principal{}, false, errMalformedAuth
		}
		if a.cfg.Auth == nil {
			return Principal{}, false, ErrUnauthorized
		}
		p, err := a.cfg.Auth.AuthenticateToken(r.Context(), tok)
		return p, err == nil, err
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return Principal{}, false, nil
	}
	if a.cfg.Auth == nil {
		return Principal{}, false, ErrUnauthorized
	}
	p, csrf, err := a.cfg.Auth.AuthenticateSession(r.Context(), c.Value)
	if err != nil {
		return Principal{}, false, err
	}
	if !safeMethod(r.Method) {
		got := r.Header.Get(CSRFHeader)
		if got == "" || csrf == "" || subtle.ConstantTimeCompare([]byte(got), []byte(csrf)) != 1 {
			return Principal{}, false, errCSRF
		}
	}
	return p, true, nil
}

// authenticate resolves the caller, applies the rate limit (per actor, or
// per IP for anonymous and failed requests) and puts the Principal into
// the request context. Only public paths may be called anonymously; the
// per-operation scope is checked later by the Huma middleware.
func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ipKey := "ip:" + IPKey(ClientIP(r, a.cfg.TrustedProxies))
		if a.isPublic(r.URL.Path) && r.Header.Get("Authorization") == "" {
			// The spec and guide need no credentials; a stale UI cookie
			// must not make them fail.
			if a.limit(w, ipKey) {
				next.ServeHTTP(w, r)
			}
			return
		}
		p, ok, err := a.credentials(r)
		switch {
		case errors.Is(err, errCSRF):
			if a.limit(w, ipKey) {
				NewError(http.StatusForbidden, CodeForbidden, err.Error(), "header."+CSRFHeader).Write(w)
			}
			return
		case errors.Is(err, errMalformedAuth):
			if a.limit(w, ipKey) {
				unauthorized(w, err.Error())
			}
			return
		case errors.Is(err, ErrUnauthorized):
			if a.limit(w, ipKey) {
				unauthorized(w, "invalid, expired or revoked credentials")
			}
			return
		case err != nil:
			a.cfg.Logger.ErrorContext(r.Context(), "api: authentication failed", "err", err)
			NewError(http.StatusInternalServerError, CodeInternal, internalMessage, "").Write(w)
			return
		case !ok:
			if a.limit(w, ipKey) {
				unauthorized(w, "authentication required: send \"Authorization: Bearer mr_...\"")
			}
			return
		}
		if !a.limit(w, p.Actor.String()) {
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="mailroom"`)
	NewError(http.StatusUnauthorized, CodeUnauthorized, msg, "").Write(w)
}

// limit applies the rate limit for key, writing the RateLimit-* headers.
// It returns false (after answering 429) when the request is refused.
func (a *API) limit(w http.ResponseWriter, key string) bool {
	d := a.limiter.Allow(key)
	d.WriteHeaders(w.Header())
	if !d.Allowed {
		ErrRateLimited().Write(w)
		return false
	}
	return true
}

// isPublic reports whether path may be requested without credentials: the
// OpenAPI documents and the agent guide.
func (a *API) isPublic(path string) bool {
	switch strings.TrimPrefix(path, Prefix) {
	case "/openapi.json", "/openapi.yaml", "/openapi-3.0.json", "/openapi-3.0.yaml", "/docs":
		return true
	}
	return false
}
