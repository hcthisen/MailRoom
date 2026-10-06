// Package contacts is the Contacts, Tags and Suppressions part of Mailroom
// (PRD §6.1, §6.2 T-1, §6.12): search and filtering, create/upsert/update,
// status changes with the §6 rules, tagging (also in bulk as a job), the
// contact timeline, GDPR export and erase (C-5, C-6), and the manual
// suppression list. It builds on contactstore, which owns the core writes
// and publishes the hooks.
package contacts

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"mailroom/internal/audit"
	"mailroom/internal/contactstore"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/jobs"
)

// Deps are the Service's dependencies.
type Deps struct {
	DB    *db.DB
	Store *contactstore.Store
	// Jobs runs bulk tagging. Submit is called inside the operation's tx.
	Jobs *jobs.Runner
	// Segments resolves segment_id filters. May be nil (tests); segment
	// filters then fail with a validation error.
	Segments core.SegmentResolver
	// TagRefs keeps saved references to tag names (segments) in step with
	// tag renames and merges, and makes deleting a referenced tag fail. May
	// be nil (tests): references are then left alone.
	TagRefs core.TagReferrer
	Clock   core.Clock   // default core.SystemClock
	Logger  *slog.Logger // default slog.Default()
}

// Service implements the contacts, tags and suppressions operations. API
// handlers and the admin UI call its methods.
type Service struct {
	db       *db.DB
	store    *contactstore.Store
	jobs     *jobs.Runner
	segments core.SegmentResolver
	tagRefs  core.TagReferrer
	clock    core.Clock
	log      *slog.Logger
}

// New returns a contacts service.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = core.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{db: d.DB, store: d.Store, jobs: d.Jobs, segments: d.Segments, tagRefs: d.TagRefs, clock: d.Clock, log: d.Logger}
}

// changeSource returns the contactstore source for writes by the actor in
// ctx: "admin" for UI users, "api" for everything else (API tokens).
func changeSource(ctx context.Context) string {
	if core.ActorFromContext(ctx).Type == core.ActorUser {
		return contactstore.SourceAdmin
	}
	return contactstore.SourceAPI
}

// consentFor returns the consent event type and source for the actor in
// ctx: ("admin", "admin:<user email>") or ("api", "api:<token name>").
// A caller-supplied origin is appended after a colon.
func consentFor(ctx context.Context, origin string) (typ, source string) {
	a := core.ActorFromContext(ctx)
	typ = contactstore.ConsentAPI
	if a.Type == core.ActorUser {
		typ = contactstore.ConsentAdmin
	}
	name := a.Name
	if name == "" {
		name = a.String()
	}
	source = typ + ":" + name
	if origin != "" {
		source += ":" + origin
	}
	return typ, source
}

// logAudit writes an audit record for the actor in ctx at the service
// clock's time, inside tx. targetID is an ID or string key; 0 and "" mean
// none.
func (s *Service) logAudit(ctx context.Context, tx *sql.Tx, op, targetType string, targetID any, changes any) error {
	id := fmt.Sprint(targetID)
	if id == "0" {
		id = ""
	}
	return audit.Write(ctx, tx, audit.Entry{
		Operation: op, TargetType: targetType, TargetID: id, Changes: changes, At: s.clock.Now(),
	})
}

// now returns the current time formatted for the database.
func (s *Service) now() string { return db.FormatTime(s.clock.Now()) }
