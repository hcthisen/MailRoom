package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// MaxUserNameLength bounds a user's display name.
const MaxUserNameLength = 200

// User is a UI user. The password hash and TOTP secret never leave the
// package.
type User struct {
	ID          int64      `json:"id" example:"1"`
	Email       string     `json:"email" format:"email" example:"owner@example.com"`
	Name        string     `json:"name" example:"Alex Owner"`
	TOTPEnabled bool       `json:"totp_enabled" doc:"Whether logins need a code from an authenticator app"`
	Version     int64      `json:"version" doc:"Increases with every change; sent as the ETag" example:"1"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// userRow is a users row including secrets.
type userRow struct {
	User
	passwordHash string
	totpSecret   string
	totpLastStep uint64 // last authenticator time step used to log in
}

const userColumns = `id, email, name, password_hash, totp_secret, totp_last_step, version, created_at, updated_at, last_login_at`

func scanUser(s interface{ Scan(...any) error }) (userRow, error) {
	var u userRow
	var totp, last sql.NullString
	var created, updated string
	var step int64
	if err := s.Scan(&u.ID, &u.Email, &u.Name, &u.passwordHash, &totp, &step, &u.Version, &created, &updated, &last); err != nil {
		return userRow{}, err
	}
	u.totpSecret = totp.String
	u.totpLastStep = uint64(max(step, 0))
	u.TOTPEnabled = totp.Valid && totp.String != ""
	var err error
	if u.CreatedAt, err = db.ParseTime(created); err != nil {
		return userRow{}, err
	}
	if u.UpdatedAt, err = db.ParseTime(updated); err != nil {
		return userRow{}, err
	}
	if u.LastLoginAt, err = db.ParseNullTime(last); err != nil {
		return userRow{}, err
	}
	return u, nil
}

func (s *Service) userRow(ctx context.Context, q db.Querier, id int64) (userRow, error) {
	u, err := scanUser(q.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return userRow{}, fmt.Errorf("user %d: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return userRow{}, fmt.Errorf("get user %d: %w", id, err)
	}
	return u, nil
}

// GetUser returns the user with id.
func (s *Service) GetUser(ctx context.Context, id int64) (User, error) {
	u, err := s.userRow(ctx, s.db.Read, id)
	return u.User, err
}

// ListUsers returns users with id > afterID in id order, at most limit.
func (s *Service) ListUsers(ctx context.Context, afterID int64, limit int) ([]User, error) {
	rows, err := s.db.Read.QueryContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u.User)
	}
	return out, rows.Err()
}

// validateName trims a display name and rejects control characters.
func validateName(v string) (string, error) {
	v = strings.TrimSpace(v)
	if utf8.RuneCountInString(v) > MaxUserNameLength {
		return v, core.Invalid("name", "must be at most %d characters", MaxUserNameLength)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return v, core.Invalid("name", "must not contain control characters")
		}
	}
	return v, nil
}

// CreateUserInput creates a user. An empty Password generates one.
type CreateUserInput struct {
	Email    string
	Name     string
	Password string
}

// CreateUser creates a UI user and audits it as "user.create". When
// in.Password is empty a random password is generated and returned (show
// it once); otherwise generated is "". Existing emails wrap ErrConflict.
func (s *Service) CreateUser(ctx context.Context, in CreateUserInput) (u User, generated string, err error) {
	email, err := core.NormalizeEmail(in.Email)
	if err != nil {
		return User{}, "", err
	}
	name, err := validateName(in.Name)
	if err != nil {
		return User{}, "", err
	}
	pw := in.Password
	if pw == "" {
		if pw, err = GeneratePassword(); err != nil {
			return User{}, "", err
		}
		generated = pw
	} else if err := CheckPasswordPolicy(pw); err != nil {
		return User{}, "", err
	}
	hash, err := s.hash(ctx, pw)
	if err != nil {
		return User{}, "", err
	}
	now := s.clock.Now()
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx,
			`INSERT INTO users (email, name, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?, ?) RETURNING id`,
			email, name, hash, db.FormatTime(now), db.FormatTime(now)).Scan(&id)
		if db.IsUniqueViolation(err) {
			return fmt.Errorf("a user with email %s already exists: %w", email, core.ErrConflict)
		}
		if err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		row, err := s.userRow(ctx, tx, id)
		if err != nil {
			return err
		}
		u = row.User
		return s.audit(ctx, tx, "user.create", "user", id, map[string]any{"email": email, "name": name})
	})
	if err != nil {
		return User{}, "", err
	}
	return u, generated, nil
}

// UpdateUserInput changes a user; nil fields are left unchanged.
type UpdateUserInput struct {
	Email    *string
	Name     *string
	Password *string
}

// UpdateUser applies in to user id. ifVersion (If-Match, 0 = skip) must
// match. A password or email change signs the user out everywhere. The
// version is bumped only when something changes.
func (s *Service) UpdateUser(ctx context.Context, id int64, in UpdateUserInput, ifVersion int64) (User, error) {
	var hash string
	if in.Password != nil {
		if err := CheckPasswordPolicy(*in.Password); err != nil {
			return User{}, err
		}
		var err error
		if hash, err = s.hash(ctx, *in.Password); err != nil {
			return User{}, err
		}
	}
	var out User
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.userRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		email, name := cur.Email, cur.Name
		if in.Email != nil {
			if email, err = core.NormalizeEmail(*in.Email); err != nil {
				return err
			}
		}
		if in.Name != nil {
			if name, err = validateName(*in.Name); err != nil {
				return err
			}
		}
		changes := audit.Diff(nil, "email", cur.Email, email)
		changes = audit.Diff(changes, "name", cur.Name, name)
		if hash != "" {
			changes["password"] = "changed"
		}
		if len(changes) == 0 {
			out = cur.User
			return nil
		}
		now := db.FormatTime(s.clock.Now())
		if hash == "" {
			hash = cur.passwordHash
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE users SET email = ?, name = ?, password_hash = ?, version = version + 1, updated_at = ? WHERE id = ?`,
			email, name, hash, now, id)
		if db.IsUniqueViolation(err) {
			return fmt.Errorf("a user with email %s already exists: %w", email, core.ErrConflict)
		}
		if err != nil {
			return fmt.Errorf("update user %d: %w", id, err)
		}
		if in.Password != nil || email != cur.Email {
			if err := endSessions(ctx, tx, id); err != nil {
				return err
			}
		}
		row, err := s.userRow(ctx, tx, id)
		if err != nil {
			return err
		}
		out = row.User
		return s.audit(ctx, tx, "user.update", "user", id, changes)
	})
	return out, err
}

// DeleteUser deletes user id and its sessions, and revokes the API tokens
// the user created (created_by "user:<id>"): tokens don't depend on their
// creator, so a departing admin's token would otherwise keep full access.
// The last remaining user can't be deleted (ErrConflict): the UI would be
// locked out.
func (s *Service) DeleteUser(ctx context.Context, id int64, ifVersion int64) error {
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.userRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
			return fmt.Errorf("count users: %w", err)
		}
		if n <= 1 {
			return fmt.Errorf("can't delete the last user: %w", core.ErrConflict)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete user %d: %w", id, err)
		}
		r, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE created_by = ? AND revoked_at IS NULL`,
			db.FormatTime(s.clock.Now()), userActor(id))
		if err != nil {
			return fmt.Errorf("revoke the tokens of user %d: %w", id, err)
		}
		revoked, _ := r.RowsAffected()
		return s.audit(ctx, tx, "user.delete", "user", id, map[string]any{"email": cur.Email, "tokens_revoked": revoked})
	})
}

// TOTPEnrollment is a new, not yet active TOTP secret.
type TOTPEnrollment struct {
	Secret     string `json:"secret" doc:"Base32 secret to enter in an authenticator app" example:"JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"`
	OtpauthURI string `json:"otpauth_uri" doc:"otpauth:// URI to show as a QR code" example:"otpauth://totp/Mailroom:owner%40example.com?secret=..."`
}

// BeginTOTP returns a fresh secret for user id without storing it. The user
// adds it to an authenticator app, and ConfirmTOTP activates it with a
// valid code, so a typo can't lock anyone out.
func (s *Service) BeginTOTP(ctx context.Context, id int64) (TOTPEnrollment, error) {
	u, err := s.GetUser(ctx, id)
	if err != nil {
		return TOTPEnrollment{}, err
	}
	secret, err := NewTOTPSecret()
	if err != nil {
		return TOTPEnrollment{}, err
	}
	return TOTPEnrollment{Secret: secret, OtpauthURI: TOTPURI(s.issuer, u.Email, secret)}, nil
}

// ConfirmTOTP turns TOTP on for user id with secret (from BeginTOTP) once
// code is valid for it now. The code can't be used again to log in, and
// the user's sessions end: they log in again with the code.
func (s *Service) ConfirmTOTP(ctx context.Context, id int64, secret, code string) (User, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return User{}, core.Invalid("secret", "is not a valid base32 TOTP secret")
	}
	secret = totpEncoding.EncodeToString(key)
	step, ok := MatchTOTP(secret, code, s.clock.Now())
	if !ok {
		return User{}, core.Invalid("code", "is not the current code for this secret; check the device clock")
	}
	return s.setTOTP(ctx, id, secret, step, 0, "user.totp_enable")
}

// ResetTOTP turns TOTP off for user id (e.g. after losing the phone) and
// ends the user's sessions. ifVersion (If-Match, 0 = skip) must match.
func (s *Service) ResetTOTP(ctx context.Context, id int64, ifVersion int64) (User, error) {
	return s.setTOTP(ctx, id, "", 0, ifVersion, "user.totp_reset")
}

// endSessions signs user id out everywhere.
func endSessions(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return fmt.Errorf("end sessions of user %d: %w", id, err)
	}
	return nil
}

func (s *Service) setTOTP(ctx context.Context, id int64, secret string, step uint64, ifVersion int64, op string) (User, error) {
	var out User
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.userRow(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET totp_secret = ?, totp_last_step = ?, version = version + 1, updated_at = ? WHERE id = ?`,
			db.NullString(secret), int64(step), db.FormatTime(s.clock.Now()), id); err != nil {
			return fmt.Errorf("set totp for user %d: %w", id, err)
		}
		if err := endSessions(ctx, tx, id); err != nil {
			return err
		}
		row, err := s.userRow(ctx, tx, id)
		if err != nil {
			return err
		}
		out = row.User
		return s.audit(ctx, tx, op, "user", id, map[string]any{"totp_enabled": secret != ""})
	})
	return out, err
}

// audit writes an audit entry with the service clock.
func (s *Service) audit(ctx context.Context, tx *sql.Tx, op, targetType string, id int64, changes any) error {
	return audit.Write(ctx, tx, audit.Entry{
		Operation: op, TargetType: targetType, TargetID: strconv.FormatInt(id, 10), Changes: changes, At: s.clock.Now(),
	})
}

// userActor is the actor string of user id, as api_tokens.created_by
// stores it.
func userActor(id int64) string {
	return core.Actor{Type: core.ActorUser, ID: strconv.FormatInt(id, 10)}.String()
}

// UserTokens returns the active (not revoked) API tokens user id created.
func (s *Service) UserTokens(ctx context.Context, id int64) ([]Token, error) {
	rows, err := s.db.Read.QueryContext(ctx,
		`SELECT `+tokenColumns+` FROM api_tokens WHERE created_by = ? AND revoked_at IS NULL ORDER BY id`, userActor(id))
	if err != nil {
		return nil, fmt.Errorf("list the tokens of user %d: %w", id, err)
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
