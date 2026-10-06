package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Session is a signed-in UI session. Token is the cookie value: it is a
// secret, stored only as its SHA-256.
type Session struct {
	Token     string
	CSRFToken string
	UserID    int64
	ExpiresAt time.Time
}

// LoginInput is a login attempt.
type LoginInput struct {
	Email     string
	Password  string
	TOTPCode  string // required when the user has TOTP on
	IP        string
	UserAgent string
}

// randomToken returns 32 random bytes, base64url-encoded.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// errTOTPReplayed: the code's time step was already used for a login.
var errTOTPReplayed = errors.New("auth: authenticator code already used")

// Login checks the credentials and starts a session. It returns
// ErrLoginLocked after too many failures (see LoginWindow), ErrBusy when
// too many logins are being checked at once, and ErrInvalidCredentials
// for everything else, including a missing, wrong or reused authenticator
// code, so a response never tells whether the password was right. Unknown
// emails cost the same time as wrong passwords.
//
// The attempt is counted against the limits before the password is
// checked, so concurrent attempts can't exceed them.
func (s *Service) Login(ctx context.Context, in LoginInput) (Session, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	attempt := s.logins.begin(email, in.IP)
	if attempt == nil {
		return Session{}, ErrLoginLocked
	}
	// notChecked takes the attempt back when no credential was judged.
	notChecked := func(err error) (Session, error) {
		attempt.cancel()
		return Session{}, err
	}
	u, err := scanUser(s.db.Read.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ?`, email))
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := VerifyPassword(ctx, in.Password, s.dummy); err != nil {
			return notChecked(err)
		}
		return Session{}, ErrInvalidCredentials
	}
	if err != nil {
		return notChecked(fmt.Errorf("login: %w", err))
	}
	ok, err := VerifyPassword(ctx, in.Password, u.passwordHash)
	if errors.Is(err, ErrBusy) || ctx.Err() != nil {
		return notChecked(err)
	}
	if err != nil {
		return notChecked(fmt.Errorf("login: user %d: %w", u.ID, err))
	}
	if !ok {
		return Session{}, ErrInvalidCredentials
	}
	var totpAttempt *reservation
	var step uint64
	if u.TOTPEnabled {
		if totpAttempt = s.logins.beginTOTP(email, in.IP); totpAttempt == nil {
			return Session{}, ErrLoginLocked
		}
		var matched bool
		step, matched = MatchTOTP(u.totpSecret, in.TOTPCode, s.clock.Now())
		if !matched || step <= u.totpLastStep {
			return Session{}, ErrInvalidCredentials
		}
	}
	sess, err := s.startSession(ctx, u.User, in, step)
	if errors.Is(err, errTOTPReplayed) {
		return Session{}, ErrInvalidCredentials
	}
	if err != nil {
		if totpAttempt != nil {
			totpAttempt.cancel()
		}
		return notChecked(err)
	}
	s.logins.succeed(email, in.IP, attempt, totpAttempt)
	return sess, nil
}

// startSession creates the session. totpStep > 0 is the time step of the
// authenticator code used; it is recorded in the same transaction, and a
// concurrent login with the same code fails with errTOTPReplayed.
func (s *Service) startSession(ctx context.Context, u User, in LoginInput, totpStep uint64) (Session, error) {
	token, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	csrf, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	now := s.clock.Now()
	sess := Session{Token: token, CSRFToken: csrf, UserID: u.ID, ExpiresAt: now.Add(SessionTTL)}
	ua := in.UserAgent
	if len(ua) > 512 {
		ua = ua[:512]
	}
	actx := core.WithActor(ctx, core.Actor{Type: core.ActorUser, ID: strconv.FormatInt(u.ID, 10), Name: u.Email})
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if totpStep > 0 {
			res, err := tx.ExecContext(ctx,
				`UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?`, int64(totpStep), u.ID, int64(totpStep))
			if err != nil {
				return fmt.Errorf("record totp step: %w", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return errTOTPReplayed
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, user_id, csrf_token, ip, user_agent, created_at, last_seen_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			sha256Hex(token), u.ID, csrf, in.IP, ua, db.FormatTime(now), db.FormatTime(now), db.FormatTime(sess.ExpiresAt)); err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, db.FormatTime(now), u.ID); err != nil {
			return fmt.Errorf("record login: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, db.FormatTime(now)); err != nil {
			return fmt.Errorf("delete expired sessions: %w", err)
		}
		return s.audit(actx, tx, "auth.login", "user", u.ID, map[string]any{"ip": in.IP})
	})
	if err != nil {
		return Session{}, err
	}
	return sess, nil
}

// Logout ends the session with the given cookie value (no error if it
// doesn't exist).
func (s *Service) Logout(ctx context.Context, cookie string) error {
	if cookie == "" {
		return nil
	}
	if _, err := s.db.Write.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sha256Hex(cookie)); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	return nil
}

// AuthenticateSession implements api.Authenticator. It returns the
// session user (scope admin: all users have the same permissions in v1)
// and the session's CSRF token, and slides the expiry forward, but never
// past SessionMaxAge after the login.
func (s *Service) AuthenticateSession(ctx context.Context, cookie string) (api.Principal, string, error) {
	if cookie == "" || len(cookie) > 128 {
		return api.Principal{}, "", api.ErrUnauthorized
	}
	id := sha256Hex(cookie)
	var userID int64
	var email, csrf, created, lastSeen, expires string
	err := s.db.Read.QueryRowContext(ctx,
		`SELECT s.user_id, u.email, s.csrf_token, s.created_at, s.last_seen_at, s.expires_at
		 FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id = ?`, id).
		Scan(&userID, &email, &csrf, &created, &lastSeen, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Principal{}, "", fmt.Errorf("unknown session: %w", api.ErrUnauthorized)
	}
	if err != nil {
		return api.Principal{}, "", fmt.Errorf("authenticate session: %w", err)
	}
	now := s.clock.Now()
	exp, err := db.ParseTime(expires)
	if err != nil {
		return api.Principal{}, "", err
	}
	start, err := db.ParseTime(created)
	if err != nil {
		return api.Principal{}, "", err
	}
	end := start.Add(SessionMaxAge)
	if !now.Before(exp) || !now.Before(end) {
		_, _ = s.db.Write.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
		return api.Principal{}, "", fmt.Errorf("session expired: %w", api.ErrUnauthorized)
	}
	if seen, err := db.ParseTime(lastSeen); err == nil && now.Sub(seen) >= touchInterval {
		newExp := now.Add(SessionTTL)
		if newExp.After(end) {
			newExp = end
		}
		if _, err := s.db.Write.ExecContext(ctx,
			`UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?`,
			db.FormatTime(now), db.FormatTime(newExp), id); err != nil {
			s.log.WarnContext(ctx, "auth: touch session", "err", err)
		}
	}
	return api.Principal{
		Actor: core.Actor{Type: core.ActorUser, ID: strconv.FormatInt(userID, 10), Name: email},
		Scope: api.ScopeAdmin,
	}, csrf, nil
}

// SetSessionCookie writes the session cookie: HttpOnly, SameSite=Lax,
// Secure when secure (the install is served over https), for SessionTTL.
func SetSessionCookie(w http.ResponseWriter, sess Session, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     api.SessionCookie,
		Value:    sess.Token,
		Path:     "/",
		MaxAge:   int(SessionTTL / time.Second),
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie removes the session cookie from the browser.
func ClearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     api.SessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
