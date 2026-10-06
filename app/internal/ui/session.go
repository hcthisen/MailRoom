package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"mailroom/internal/api"
	"mailroom/internal/auth"
	"mailroom/internal/core"
)

// CSRFField is the form field that carries the session's CSRF token on
// plain form posts. htmx requests send it as the api.CSRFHeader header
// (set on <body> with hx-headers by the layout).
const CSRFField = "csrf_token"

// loginCSRFCookie holds the login form's double-submit token: there is no
// session yet whose token the form could carry.
const loginCSRFCookie = "mailroom_login_csrf"

// Session is the signed-in user of a request.
type Session struct {
	UserID int64
	Email  string
	CSRF   string
	Actor  core.Actor
}

// reqState is the per-request state the middleware keeps in the context.
type reqState struct {
	session *Session
	flashes []Flash // set during this request (Flash)
	read    bool    // the flash cookie was consumed
}

type stateKey struct{}

func stateOf(r *http.Request) *reqState {
	if st, ok := r.Context().Value(stateKey{}).(*reqState); ok {
		return st
	}
	return &reqState{}
}

// SessionFrom returns the request's session, or nil on public pages.
func SessionFrom(r *http.Request) *Session { return stateOf(r).session }

// isHTMX reports whether r was sent by htmx.
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// safeMethod reports whether m never changes state.
func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// base wraps every UI handler: security headers, the request state, the
// body limit and error rendering.
func (u *UI) base(a Action, h HandlerFunc) http.Handler {
	limit := a.MaxBodyBytes
	if limit <= 0 {
		limit = DefaultMaxBody
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		r = r.WithContext(context.WithValue(r.Context(), stateKey{}, &reqState{}))
		if r.Body != nil && !safeMethod(r.Method) {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		defer func() {
			// A multipart body parsed for the CSRF check may have spilled
			// files to disk (the server only cleans up its own request).
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
		if err := h(w, r); err != nil {
			u.Error(w, r, err)
		}
	})
}

// setSecurityHeaders sets the admin UI's headers. They replace the
// server's defaults (set before the handler runs).
//
// The CSP allows scripts only from the UI's own origin (htmx.min.js,
// app.js: no inline scripts, no eval). Inline styles and https images are
// allowed because email previews (srcdoc iframes, which inherit this
// policy) are full of them; the iframes are sandboxed without
// allow-scripts, so an email can never run code.
func setSecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
		"img-src 'self' data: https:; font-src 'self'; connect-src 'self'; frame-src 'self'; "+
		"form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
}

// errCSRF is a non-GET request without the session's CSRF token.
var errCSRF = errors.New("the form has expired or did not come from this site: reload the page and try again")

// requireSession authenticates the session cookie and checks the CSRF
// token on non-GET requests. Logged-out requests go to the login page.
func (u *UI) requireSession(next HandlerFunc) HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		c, err := r.Cookie(api.SessionCookie)
		if err != nil || c.Value == "" {
			u.toLogin(w, r)
			return nil
		}
		p, csrf, err := u.d.Auth.AuthenticateSession(r.Context(), c.Value)
		if errors.Is(err, api.ErrUnauthorized) {
			auth.ClearSessionCookie(w, u.secure)
			u.toLogin(w, r)
			return nil
		}
		if err != nil {
			return err
		}
		if !safeMethod(r.Method) && !u.validCSRF(r, csrf) {
			u.renderError(w, r, http.StatusForbidden, "Request refused", errCSRF.Error())
			return nil
		}
		id, _ := strconv.ParseInt(p.Actor.ID, 10, 64)
		st := stateOf(r)
		st.session = &Session{UserID: id, Email: p.Actor.Name, CSRF: csrf, Actor: p.Actor}
		ctx := api.WithPrincipal(r.Context(), p)
		return next(w, r.WithContext(ctx))
	}
}

// validCSRF checks the X-CSRF-Token header, else the csrf_token form
// field, against want.
func (u *UI) validCSRF(r *http.Request, want string) bool {
	got := r.Header.Get(api.CSRFHeader)
	if got == "" {
		got = formToken(r)
	}
	return want != "" && got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// formToken reads CSRFField from a urlencoded or multipart body. For a
// multipart body this parses the form (files up to 32 MiB in memory, the
// rest in temp files), so the handler finds r.MultipartForm ready.
func formToken(r *http.Request) string {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch ct {
	case "application/x-www-form-urlencoded":
		return r.PostFormValue(CSRFField)
	case "multipart/form-data":
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return ""
		}
		return r.PostFormValue(CSRFField)
	}
	return ""
}

// toLogin sends a logged-out request to the login page, remembering where
// it wanted to go (GET only).
func (u *UI) toLogin(w http.ResponseWriter, r *http.Request) {
	target := Prefix + "/login"
	if r.Method == http.MethodGet && r.URL.Path != Prefix+"/" {
		target += "?next=" + url.QueryEscape(r.URL.RequestURI())
	}
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// localNext returns next when it is a path inside the UI, else the
// dashboard, so the login form can't redirect elsewhere.
func localNext(next string) string {
	if !strings.HasPrefix(next, Prefix+"/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") ||
		strings.HasPrefix(next, Prefix+"/login") {
		return Prefix + "/"
	}
	return next
}

// loginView is the login page data.
type loginView struct {
	Next  string
	Token string // double-submit CSRF token
	Form  *Form
}

func (u *UI) registerAuth() {
	u.handle(Action{Method: http.MethodGet, Path: Prefix + "/login", Public: true,
		UIOnly: "session sign-in form: the API authenticates with tokens, not passwords", Handler: u.loginPage})
	u.handle(Action{Method: http.MethodPost, Path: Prefix + "/login", Public: true, MaxBodyBytes: 16 << 10,
		UIOnly: "session sign-in (auth.Login): the API authenticates with tokens, not passwords", Handler: u.login})
	u.handle(Action{Method: http.MethodPost, Path: Prefix + "/logout", MaxBodyBytes: 16 << 10,
		UIOnly: "ends the browser session (auth.Logout); API tokens are revoked with revoke-token", Handler: u.logout})
}

// loginToken returns the login form's double-submit token, setting the
// cookie when there is none yet.
func (u *UI) loginToken(w http.ResponseWriter, r *http.Request) (string, error) {
	if c, err := r.Cookie(loginCSRFCookie); err == nil && len(c.Value) >= 32 && len(c.Value) <= 64 {
		return c.Value, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: loginCSRFCookie, Value: tok, Path: Prefix + "/login",
		MaxAge: 3600, HttpOnly: true, Secure: u.secure, SameSite: http.SameSiteStrictMode})
	return tok, nil
}

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(api.SessionCookie); err == nil && c.Value != "" {
		if _, _, err := u.d.Auth.AuthenticateSession(r.Context(), c.Value); err == nil {
			http.Redirect(w, r, localNext(r.URL.Query().Get("next")), http.StatusSeeOther)
			return nil
		}
	}
	tok, err := u.loginToken(w, r)
	if err != nil {
		return err
	}
	return u.Page(w, r, "auth/login", View{Title: "Sign in", Bare: true,
		Data: loginView{Next: localNext(r.URL.Query().Get("next")), Token: tok, Form: EmptyForm()}})
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) error {
	if err := r.ParseForm(); err != nil {
		return core.Invalid("", "the form could not be read")
	}
	c, err := r.Cookie(loginCSRFCookie)
	if err != nil || c.Value == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.PostFormValue(CSRFField))) != 1 {
		u.renderError(w, r, http.StatusForbidden, "Request refused",
			"The sign-in form has expired or did not come from this site. Open the sign-in page again.")
		return nil
	}
	form := NewForm(r)
	next := localNext(form.Get("next"))
	sess, err := u.d.Auth.Login(r.Context(), auth.LoginInput{
		Email:     form.Get("email"),
		Password:  r.PostFormValue("password"),
		TOTPCode:  strings.ReplaceAll(form.Get("totp_code"), " ", ""),
		IP:        api.ClientIP(r, u.d.Config.TrustedProxies),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		status := http.StatusUnauthorized
		switch {
		case errors.Is(err, auth.ErrLoginLocked):
			status = http.StatusTooManyRequests
			form.Message = err.Error()
		case errors.Is(err, auth.ErrBusy):
			status = http.StatusServiceUnavailable
			form.Message = err.Error()
		case errors.Is(err, auth.ErrInvalidCredentials):
			form.Message = "Invalid email, password or authenticator code."
		default:
			return err
		}
		return u.PageStatus(w, r, status, "auth/login", View{Title: "Sign in", Bare: true,
			Data: loginView{Next: next, Token: c.Value, Form: form}})
	}
	auth.SetSessionCookie(w, sess, u.secure)
	http.SetCookie(w, &http.Cookie{Name: loginCSRFCookie, Value: "", Path: Prefix + "/login", MaxAge: -1,
		HttpOnly: true, Secure: u.secure, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, next, http.StatusSeeOther)
	return nil
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(api.SessionCookie); err == nil {
		if err := u.d.Auth.Logout(r.Context(), c.Value); err != nil {
			return err
		}
	}
	auth.ClearSessionCookie(w, u.secure)
	u.Flash(r, FlashSuccess, "You are signed out.")
	u.Redirect(w, r, Prefix+"/login")
	return nil
}

// csrfHeaders is the hx-headers value of the layout's <body>.
func csrfHeaders(token string) string {
	b, _ := json.Marshal(map[string]string{api.CSRFHeader: token})
	return string(b)
}
