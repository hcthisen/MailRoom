// Package stats computes the install-wide numbers of the admin dashboard
// (PRD A-3) and serves them at GET /api/v1/stats/dashboard, so the
// dashboard shows nothing the API can't (G-1): new contacts in the last 7
// and 30 days, unsubscribes in the last 30 days, emails sent this month
// and the hard-bounce and complaint rates of the last 30 days.
//
// It only reads. The SES quota, the sending state and the doctor warnings
// the dashboard also shows come from the system and sending packages.
package stats

import (
	"context"
	"fmt"
	"sync"
	"time"

	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/settings"
)

// CacheTTL is how long a computed Dashboard is reused: the numbers scan
// the messages table, and the dashboard is reloaded often.
const CacheTTL = time.Minute

// countedKinds are the message kinds that count as "emails sent": what
// contacts receive. Test sends and owner alerts are left out.
const countedKinds = `('broadcast', 'sequence', 'optin')`

// Deps are the Service's dependencies.
type Deps struct {
	DB    *db.DB
	Clock core.Clock // default core.SystemClock
	// Settings supplies the time zone that "this month" is measured in
	// (nil: UTC).
	Settings *settings.Service
}

// Service computes the dashboard numbers.
type Service struct {
	db       *db.DB
	clock    core.Clock
	settings *settings.Service

	mu     sync.Mutex
	cached *Dashboard
	stamp  contactStamp // of cached
}

// contactStamp identifies the state of the contact numbers: the contact
// count, the active count and the latest status change. All three come
// from indexes, so checking it is cheap. A cached Dashboard is reused only
// while it is unchanged, so an import, an erase or a status change shows
// on the dashboard at once, not only after CacheTTL.
type contactStamp struct {
	total, active, lastChange int64
}

func (s *Service) contactStamp(ctx context.Context) (contactStamp, error) {
	var st contactStamp
	err := s.db.Read.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM contacts),
		(SELECT count(*) FROM contacts WHERE status = 'active'),
		(SELECT coalesce(max(id), 0) FROM contact_status_changes)`).Scan(&st.total, &st.active, &st.lastChange)
	if err != nil {
		return st, fmt.Errorf("stats: contact stamp: %w", err)
	}
	return st, nil
}

// New returns a stats service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	return &Service{db: d.DB, clock: d.Clock, settings: d.Settings}
}

// Dashboard is the answer of GET /stats/dashboard.
type Dashboard struct {
	GeneratedAt time.Time `json:"generated_at" doc:"When the numbers were computed. They are cached for up to a minute, but recomputed at once when contacts are added, erased or change status, so the contact numbers are always current"`
	Timezone    string    `json:"timezone" doc:"The settings time zone; this_month starts at midnight on the 1st there" example:"Europe/London"`

	ContactsTotal   int64 `json:"contacts_total" doc:"All contacts, whatever their status" example:"5230"`
	ContactsActive  int64 `json:"contacts_active" doc:"Contacts with status active (they receive broadcasts and sequences)" example:"4980"`
	NewContacts7d   int64 `json:"new_contacts_7d" doc:"Contacts created in the last 7 days" example:"42"`
	NewContacts30d  int64 `json:"new_contacts_30d" doc:"Contacts created in the last 30 days" example:"180"`
	Unsubscribes30d int64 `json:"unsubscribes_30d" doc:"Contacts whose status changed to unsubscribed in the last 30 days" example:"12"`

	MonthStart    time.Time `json:"month_start" doc:"Start of this month in the settings time zone"`
	SentThisMonth int64     `json:"sent_this_month" doc:"Broadcast, sequence and opt-in emails sent since month_start (test sends and alerts are not counted)" example:"9200"`
	Sent30d       int64     `json:"sent_30d" doc:"Broadcast, sequence and opt-in emails sent in the last 30 days" example:"12050"`

	HardBounces30d   int64   `json:"hard_bounces_30d" doc:"Emails sent in the last 30 days that bounced permanently" example:"30"`
	Complaints30d    int64   `json:"complaints_30d" doc:"Emails sent in the last 30 days that were reported as spam" example:"1"`
	BounceRate30d    float64 `json:"bounce_rate_30d" doc:"hard_bounces_30d / sent_30d (0 when nothing was sent)" example:"0.0025"`
	ComplaintRate30d float64 `json:"complaint_rate_30d" doc:"complaints_30d / sent_30d (0 when nothing was sent)" example:"0.00008"`
}

// Dashboard returns the dashboard numbers, computed at most once per
// CacheTTL while the contacts don't change (contactStamp): the contact
// numbers are always current, the message numbers at most CacheTTL old.
func (s *Service) Dashboard(ctx context.Context) (Dashboard, error) {
	now := s.clock.Now()
	stamp, err := s.contactStamp(ctx)
	if err != nil {
		return Dashboard{}, err
	}
	s.mu.Lock()
	if c := s.cached; c != nil && s.stamp == stamp && now.Sub(c.GeneratedAt) < CacheTTL && !now.Before(c.GeneratedAt) {
		d := *c
		s.mu.Unlock()
		return d, nil
	}
	s.mu.Unlock()

	d, err := s.compute(ctx, now)
	if err != nil {
		return Dashboard{}, err
	}
	s.mu.Lock()
	s.cached, s.stamp = &d, stamp
	s.mu.Unlock()
	return d, nil
}

// Invalidate drops the cached numbers (tests, or after a bulk change).
func (s *Service) Invalidate() {
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()
}

func (s *Service) compute(ctx context.Context, now time.Time) (Dashboard, error) {
	loc := time.UTC
	if s.settings != nil {
		if st, err := s.settings.Get(ctx); err == nil {
			loc = st.Location()
		}
	}
	local := now.In(loc)
	monthStart := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
	d7 := db.FormatTime(now.Add(-7 * 24 * time.Hour))
	d30 := db.FormatTime(now.Add(-30 * 24 * time.Hour))
	month := db.FormatTime(monthStart)

	d := Dashboard{GeneratedAt: now.UTC(), Timezone: loc.String(), MonthStart: monthStart.UTC()}
	q := s.db.Read
	if err := q.QueryRowContext(ctx, `
		SELECT count(*),
		  IFNULL(sum(status = 'active'), 0),
		  IFNULL(sum(created_at >= ?), 0),
		  IFNULL(sum(created_at >= ?), 0)
		FROM contacts`, d7, d30).
		Scan(&d.ContactsTotal, &d.ContactsActive, &d.NewContacts7d, &d.NewContacts30d); err != nil {
		return Dashboard{}, fmt.Errorf("stats: contacts: %w", err)
	}
	if err := q.QueryRowContext(ctx, `
		SELECT count(DISTINCT contact_id) FROM contact_status_changes
		WHERE to_status = 'unsubscribed' AND created_at >= ?`, d30).Scan(&d.Unsubscribes30d); err != nil {
		return Dashboard{}, fmt.Errorf("stats: unsubscribes: %w", err)
	}
	if err := q.QueryRowContext(ctx, `
		SELECT IFNULL(sum(sent_at >= ?), 0), IFNULL(sum(sent_at >= ?), 0)
		FROM messages WHERE sent_at IS NOT NULL AND kind IN `+countedKinds+` AND sent_at >= min(?, ?)`,
		month, d30, month, d30).Scan(&d.SentThisMonth, &d.Sent30d); err != nil {
		return Dashboard{}, fmt.Errorf("stats: sent: %w", err)
	}
	// Bounces and complaints of the messages sent in the window, so the
	// rates compare like with like.
	if err := q.QueryRowContext(ctx, `
		SELECT
		  count(DISTINCT CASE WHEN e.type = 'bounce' AND e.subtype = 'permanent' THEN e.message_id END),
		  count(DISTINCT CASE WHEN e.type = 'complaint' AND e.subtype <> 'not-spam' THEN e.message_id END)
		FROM events e JOIN messages m ON m.id = e.message_id
		WHERE e.type IN ('bounce', 'complaint') AND e.created_at >= ?
		  AND m.sent_at >= ? AND m.kind IN `+countedKinds, d30, d30).
		Scan(&d.HardBounces30d, &d.Complaints30d); err != nil {
		return Dashboard{}, fmt.Errorf("stats: bounces: %w", err)
	}
	if d.Sent30d > 0 {
		d.BounceRate30d = float64(d.HardBounces30d) / float64(d.Sent30d)
		d.ComplaintRate30d = float64(d.Complaints30d) / float64(d.Sent30d)
	}
	return d, nil
}
