package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/config"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// Deps are the Service's dependencies.
type Deps struct {
	DB    *db.DB
	Clock core.Clock
	// Domain is the configured sending domain (config.Config.Domain).
	Domain string
}

// Service reads and writes settings. Get is cached in memory and the cache
// is refreshed by every write through the Service.
type Service struct {
	db     *db.DB
	clock  core.Clock
	domain string

	mu     sync.RWMutex
	cached *Settings
	gen    uint64 // bumped by every invalidation, so a slow load can't cache stale data
}

// New returns a settings service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	return &Service{db: d.DB, clock: d.Clock, domain: strings.ToLower(d.Domain)}
}

// Domain returns the read-only sending domain.
func (s *Service) Domain() string { return s.domain }

// reservedKeys can't be written through PutRaw.
func reserved(key string) bool { return key == Key }

// SeedFromConfig stores the initial settings from the environment when no
// settings exist yet (first start) and reports whether it did. Existing
// settings are never overwritten: after the first start the API owns them.
func (s *Service) SeedFromConfig(ctx context.Context, cfg config.Config) (bool, error) {
	seed := Defaults()
	seed.Sender = Sender{FromName: cfg.FromName, FromEmail: cfg.FromEmail, ReplyTo: cfg.ReplyToEmail}
	seed.CompanyFooter = cfg.CompanyFooter
	seed.OwnerEmail = cfg.OwnerEmail
	if cfg.Timezone != "" {
		seed.Timezone = cfg.Timezone
	}
	seed, err := Validate(seed, s.domain)
	if err != nil {
		return false, fmt.Errorf("settings: seed from environment: %w", err)
	}
	seed.Version = 1
	seed.Domain = s.domain
	var seeded bool
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, found, err := s.load(ctx, tx); err != nil || found {
			return err
		}
		if err := s.store(ctx, tx, seed); err != nil {
			return err
		}
		seeded = true
		return audit.Write(core.WithActor(ctx, core.Actor{Type: core.ActorSystem}), tx, audit.Entry{
			Operation: "settings.seed", TargetType: "settings", TargetID: Key, At: s.clock.Now(),
		})
	})
	if err != nil {
		return false, err
	}
	s.invalidate()
	return seeded, nil
}

// Get returns the current settings (Defaults with the configured domain
// if nothing was stored yet).
func (s *Service) Get(ctx context.Context) (Settings, error) {
	s.mu.RLock()
	c, gen := s.cached, s.gen
	s.mu.RUnlock()
	if c != nil {
		return c.clone(), nil
	}
	st, found, err := s.load(ctx, s.db.Read)
	if err != nil {
		return Settings{}, err
	}
	if !found {
		st = Defaults()
	}
	st.Domain = s.domain
	s.mu.Lock()
	if s.gen == gen {
		cp := st.clone()
		s.cached = &cp
	}
	s.mu.Unlock()
	return st, nil
}

func (s *Service) invalidate() {
	s.mu.Lock()
	s.cached = nil
	s.gen++
	s.mu.Unlock()
}

// Update applies p to the settings, validates the result and stores it
// with an audit record of the changed fields. ifVersion (from If-Match)
// must match the current version unless it is 0. A patch that changes
// nothing returns the current settings without a new version.
func (s *Service) Update(ctx context.Context, p Patch, ifVersion int64) (Settings, error) {
	var out Settings
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		cur, found, err := s.load(ctx, tx)
		if err != nil {
			return err
		}
		if !found {
			cur = Defaults()
		}
		cur.Domain = s.domain
		if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
			return err
		}
		next, err := Validate(p.apply(cur), s.domain)
		if err != nil {
			return err
		}
		if p.CompanyFooter != nil && next.CompanyFooter == "" {
			return core.Invalid("company_footer", "can't be empty: every marketing email must show the sender's name and postal address")
		}
		changes, err := diff(cur, next)
		if err != nil {
			return err
		}
		if len(changes) == 0 && found {
			out = cur
			return nil
		}
		next.Version = cur.Version + 1
		if err := s.store(ctx, tx, next); err != nil {
			return err
		}
		out = next
		return audit.Write(ctx, tx, audit.Entry{
			Operation: "settings.update", TargetType: "settings", TargetID: Key, Changes: changes, At: s.clock.Now(),
		})
	})
	if err != nil {
		return Settings{}, err
	}
	s.invalidate()
	return out, nil
}

func (s *Service) load(ctx context.Context, q db.Querier) (Settings, bool, error) {
	raw, err := s.GetRaw(ctx, q, Key)
	if errors.Is(err, core.ErrNotFound) {
		return Settings{}, false, nil
	}
	if err != nil {
		return Settings{}, false, err
	}
	st := Defaults()
	if err := json.Unmarshal(raw, &st); err != nil {
		return Settings{}, false, fmt.Errorf("settings: decode %q: %w", Key, err)
	}
	st.Domain = s.domain
	return st, true, nil
}

func (s *Service) store(ctx context.Context, q db.Querier, st Settings) error {
	st.Domain = s.domain
	return s.put(ctx, q, Key, st)
}

// GetRaw returns the JSON document stored under key. A missing key wraps
// core.ErrNotFound. A nil q reads from the read pool.
func (s *Service) GetRaw(ctx context.Context, q db.Querier, key string) (json.RawMessage, error) {
	if q == nil {
		q = s.db.Read
	}
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("setting %q: %w", key, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("settings: read %q: %w", key, err)
	}
	return json.RawMessage(v), nil
}

// PutRaw stores value (any JSON-encodable value, or json.RawMessage) under
// key, for packages that keep their own settings document. The "app" key
// belongs to this package and is refused. Pass the caller's transaction so
// the write joins its audit record.
func (s *Service) PutRaw(ctx context.Context, q db.Querier, key string, value any) error {
	if key == "" {
		return core.Invalid("key", "is required")
	}
	if reserved(key) {
		return fmt.Errorf("settings: key %q is reserved: %w", key, core.ErrForbidden)
	}
	return s.put(ctx, q, key, value)
}

func (s *Service) put(ctx context.Context, q db.Querier, key string, value any) error {
	var v string
	switch x := value.(type) {
	case json.RawMessage:
		if !json.Valid(x) {
			return core.Invalid("value", "is not valid JSON")
		}
		v = string(x)
	default:
		b, err := db.MarshalJSON(value)
		if err != nil {
			return err
		}
		v = b
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, v, db.FormatTime(s.clock.Now())); err != nil {
		return fmt.Errorf("settings: write %q: %w", key, err)
	}
	return nil
}
