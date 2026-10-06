package sequences

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mailroom/internal/api"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/jobs"
)

// EnrollmentFilter selects enrollments for ListEnrollments.
type EnrollmentFilter struct {
	Status   string // "" = any
	BeforeID int64  // keyset cursor: only IDs below this (0 = from the newest)
	Limit    int    // maximum rows (callers pass page size + 1)
}

const enrollmentSelect = `
	SELECT e.id, e.sequence_id, e.contact_id, c.email, e.status, e.next_step_id, st.position, e.next_run_at,
	       e.exit_reason, e.source, e.started_at, e.updated_at, e.finished_at
	FROM enrollments e JOIN contacts c ON c.id = e.contact_id
	LEFT JOIN sequence_steps st ON st.id = e.next_step_id`

func scanEnrollment(sc interface{ Scan(...any) error }) (SequenceEnrollment, error) {
	var (
		e                         SequenceEnrollment
		nextStep                  sql.NullInt64
		position                  sql.NullInt64
		nextRun, finished         sql.NullString
		started, updated, emailAt string
	)
	if err := sc.Scan(&e.ID, &e.SequenceID, &e.ContactID, &emailAt, &e.Status, &nextStep, &position, &nextRun,
		&e.ExitReason, &e.Source, &started, &updated, &finished); err != nil {
		return SequenceEnrollment{}, err
	}
	e.Email = emailAt
	if nextStep.Valid {
		id := nextStep.Int64
		e.NextStepID = &id
	}
	if position.Valid {
		p := int(position.Int64)
		e.NextStepPosition = &p
	}
	var err error
	if e.NextRunAt, err = db.ParseNullTime(nextRun); err != nil {
		return SequenceEnrollment{}, err
	}
	if e.FinishedAt, err = db.ParseNullTime(finished); err != nil {
		return SequenceEnrollment{}, err
	}
	if e.StartedAt, err = db.ParseTime(started); err != nil {
		return SequenceEnrollment{}, err
	}
	if e.UpdatedAt, err = db.ParseTime(updated); err != nil {
		return SequenceEnrollment{}, err
	}
	return e, nil
}

// getEnrollment reads one enrollment of a sequence; unknown IDs (or one of
// another sequence) wrap core.ErrNotFound.
func getEnrollment(ctx context.Context, q db.Querier, seqID, id int64) (SequenceEnrollment, error) {
	e, err := scanEnrollment(q.QueryRowContext(ctx, enrollmentSelect+` WHERE e.id = ? AND e.sequence_id = ?`, id, seqID))
	if errors.Is(err, sql.ErrNoRows) {
		return SequenceEnrollment{}, fmt.Errorf("enrollment %d of sequence %d: %w", id, seqID, core.ErrNotFound)
	}
	if err != nil {
		return SequenceEnrollment{}, fmt.Errorf("sequences: enrollment %d: %w", id, err)
	}
	return e, nil
}

// ListEnrollments returns the enrollments of a sequence, newest first.
func (s *Service) ListEnrollments(ctx context.Context, seqID int64, f EnrollmentFilter) ([]SequenceEnrollment, error) {
	if _, err := loadSeq(ctx, s.db.Read, seqID); err != nil {
		return nil, err
	}
	if f.Limit <= 0 {
		f.Limit = api.DefaultLimit
	}
	q := enrollmentSelect + ` WHERE e.sequence_id = ?`
	args := []any{seqID}
	if f.Status != "" {
		q += ` AND e.status = ?`
		args = append(args, f.Status)
	}
	if f.BeforeID > 0 {
		q += ` AND e.id < ?`
		args = append(args, f.BeforeID)
	}
	q += ` ORDER BY e.id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.Read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sequences: list enrollments: %w", err)
	}
	defer rows.Close()
	var out []SequenceEnrollment
	for rows.Next() {
		e, err := scanEnrollment(rows)
		if err != nil {
			return nil, fmt.Errorf("sequences: list enrollments: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EnrollRequest enrols contacts given by ID or by email address.
type EnrollRequest struct {
	ContactIDs []int64    `json:"contact_ids,omitempty" maxItems:"50000" doc:"Contacts to enrol; give this or emails" example:"[42,43]"`
	Emails     []string   `json:"emails,omitempty" maxItems:"50000" doc:"Addresses of existing contacts to enrol; give this or contact_ids" example:"[\"alice@example.com\"]"`
	AtStep     int        `json:"at_step,omitempty" minimum:"0" maximum:"100" doc:"Start at this step position (0 or 1 = the first step); for contacts migrated mid-drip" example:"3"`
	StartAt    *time.Time `json:"start_at,omitempty" doc:"The starting step is due at start_at plus its delay (default: now plus its delay); may be in the past"`
	Source     string     `json:"source,omitempty" maxLength:"100" doc:"Recorded on the enrollments (default api), e.g. migration" example:"migration"`
}

// EnrollResult is the outcome of an enrolment request.
type EnrollResult struct {
	DryRun        bool           `json:"dry_run" doc:"true when nothing was changed"`
	WouldEnroll   int            `json:"would_enroll" doc:"Contacts that are (or, in a dry run, would be) enrolled" example:"120"`
	Enrolled      int            `json:"enrolled" doc:"Enrollments created; 0 in a dry run and while a job does the work" example:"120"`
	Skipped       map[string]int `json:"skipped" doc:"Contacts not enrolled, by reason: not_found, not_active, already_active, reentry, exit_tag, no_steps, unsubscribed" example:"{\"already_active\":2}"`
	EnrollmentIDs []int64        `json:"enrollment_ids,omitempty" doc:"IDs of the new enrollments (requests of up to 500 contacts)"`
	Job           *jobs.Job      `json:"job,omitempty" doc:"The job doing the work, for more than 500 contacts"`
}

// normalizeRequest validates req, de-duplicates it and returns its size.
func normalizeRequest(req *EnrollRequest) (int, error) {
	if (len(req.ContactIDs) > 0) == (len(req.Emails) > 0) {
		return 0, core.Invalid("contact_ids", "give either contact_ids or emails")
	}
	if len(req.ContactIDs)+len(req.Emails) > MaxEnrollBatch {
		return 0, core.Invalid("contact_ids", "at most %d contacts per request", MaxEnrollBatch)
	}
	if req.AtStep < 0 {
		return 0, core.Invalid("at_step", "must be 0 or a step position")
	}
	req.Source = strings.TrimSpace(req.Source)
	if req.Source == "" {
		req.Source = "api"
	}
	if utf8.RuneCountInString(req.Source) > 100 || strings.IndexFunc(req.Source, unicode.IsControl) >= 0 {
		return 0, core.Invalid("source", "must be a single line of at most 100 characters")
	}
	ids := slices.Clone(req.ContactIDs)
	slices.Sort(ids)
	req.ContactIDs = slices.Compact(ids)
	if len(req.ContactIDs) > 0 && req.ContactIDs[0] <= 0 {
		return 0, core.Invalid("contact_ids", "IDs must be positive")
	}
	emails := make([]string, 0, len(req.Emails))
	seen := make(map[string]bool, len(req.Emails))
	for i, raw := range req.Emails {
		e, err := core.NormalizeEmail(raw)
		if err != nil {
			return 0, core.Invalid(fmt.Sprintf("emails[%d]", i), "is not a valid email address")
		}
		if !seen[e] {
			seen[e] = true
			emails = append(emails, e)
		}
	}
	req.Emails = emails
	return len(req.ContactIDs) + len(req.Emails), nil
}

// enrollJob is the parameters of a sequences.enroll job.
type enrollJob struct {
	SequenceID  int64         `json:"sequence_id"`
	Request     EnrollRequest `json:"request"`
	SubmittedAt time.Time     `json:"submitted_at"`
}

// EnrollContacts enrols contacts in a sequence (S-2, S-8). Up to
// EnrollJobThreshold contacts are enrolled at once; more are handed to a
// "sequences.enroll" job, whose result has the same shape. With dryRun
// nothing changes (also above the threshold) and the result says what
// would happen; a dry run only reads (see previewEnroll), so even one of
// MaxEnrollBatch contacts never holds the write connection.
func (s *Service) EnrollContacts(ctx context.Context, seqID int64, req EnrollRequest, dryRun bool) (EnrollResult, error) {
	n, err := normalizeRequest(&req)
	if err != nil {
		return EnrollResult{}, err
	}
	if dryRun {
		return s.previewEnroll(ctx, seqID, req)
	}
	res := EnrollResult{Skipped: map[string]int{}}
	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := loadSeq(ctx, tx, seqID); err != nil {
			return err
		}
		if _, _, err := startStep(ctx, tx, seqID, req.AtStep); err != nil {
			return err
		}
		audit := map[string]any{"contacts": n, "at_step": req.AtStep, "start_at": req.StartAt, "source": req.Source}
		if n > EnrollJobThreshold {
			if s.jobs == nil {
				return errors.New("sequences: no job runner configured")
			}
			j, err := s.jobs.Submit(ctx, tx, JobEnroll, enrollJob{SequenceID: seqID, Request: req, SubmittedAt: s.now()})
			if err != nil {
				return err
			}
			res.Job = &j
			audit["job_id"] = j.ID
			return s.logAudit(ctx, tx, "sequence.enroll", "sequence", seqID, audit)
		}
		if err := s.enrollBatch(ctx, tx, seqID, req, nil, &res); err != nil {
			return err
		}
		audit["enrolled"], audit["skipped"] = res.Enrolled, res.Skipped
		return s.logAudit(ctx, tx, "sequence.enroll", "sequence", seqID, audit)
	})
	if err != nil {
		return EnrollResult{}, err
	}
	if n > EnrollJobThreshold {
		res.EnrollmentIDs = nil
	}
	return res, nil
}

// previewEnroll answers a dry run of EnrollContacts from one read-only
// snapshot on the read pool: the same checks as enrolling (sequence,
// at_step, contact status, re-entry, exit tags), without an INSERT and
// without taking the single write connection. The request is already
// de-duplicated, so no contact can block another one of the same request.
func (s *Service) previewEnroll(ctx context.Context, seqID int64, req EnrollRequest) (EnrollResult, error) {
	res := EnrollResult{DryRun: true, Skipped: map[string]int{}}
	tx, err := s.db.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return EnrollResult{}, fmt.Errorf("sequences: begin read: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // read-only: nothing to commit
	seq, err := loadSeq(ctx, tx, seqID)
	if err != nil {
		return EnrollResult{}, err
	}
	_, hasSteps, err := startStep(ctx, tx, seqID, req.AtStep)
	if err != nil {
		return EnrollResult{}, err
	}
	ids := req.ContactIDs
	if len(req.Emails) > 0 {
		found, missing, err := contactsByEmail(ctx, tx, req.Emails)
		if err != nil {
			return EnrollResult{}, err
		}
		ids = found
		if missing > 0 {
			res.Skipped[SkipNotFound] += missing
		}
	}
	for _, cid := range ids {
		if err := ctx.Err(); err != nil {
			return EnrollResult{}, err
		}
		if !hasSteps {
			res.Skipped[SkipNoSteps]++
			continue
		}
		reason, err := enrollBlocker(ctx, tx, seq, cid)
		if err != nil {
			return EnrollResult{}, err
		}
		if reason != "" {
			res.Skipped[reason]++
			continue
		}
		res.WouldEnroll++
	}
	return res, nil
}

// enrollBatch enrols the contacts of req in tx and adds the outcome to
// res. done, when set, reports contacts an earlier (interrupted) run of
// the same job already enrolled; they count as enrolled.
func (s *Service) enrollBatch(ctx context.Context, tx *sql.Tx, seqID int64, req EnrollRequest, done func(contactID int64) (bool, error), res *EnrollResult) error {
	ids := req.ContactIDs
	if len(req.Emails) > 0 {
		found, missing, err := contactsByEmail(ctx, tx, req.Emails)
		if err != nil {
			return err
		}
		ids = found
		if missing > 0 {
			res.Skipped[SkipNotFound] += missing
		}
	}
	opts := core.EnrollOptions{Source: req.Source, AtStep: req.AtStep, StartAt: req.StartAt}
	for _, cid := range ids {
		if done != nil {
			ok, err := done(cid)
			if err != nil {
				return err
			}
			if ok {
				res.WouldEnroll++
				res.Enrolled++
				continue
			}
		}
		id, reason, err := s.enroll(ctx, tx, seqID, cid, opts)
		if err != nil {
			return err
		}
		if reason != "" {
			res.Skipped[reason]++
			continue
		}
		res.WouldEnroll++
		res.Enrolled++
		res.EnrollmentIDs = append(res.EnrollmentIDs, id)
	}
	return nil
}

// contactsByEmail resolves normalised addresses to contact IDs and counts
// those without a contact.
func contactsByEmail(ctx context.Context, q db.Querier, emails []string) ([]int64, int, error) {
	var ids []int64
	for chunk := range slices.Chunk(emails, 500) {
		args := make([]any, len(chunk))
		for i, e := range chunk {
			args[i] = e
		}
		rows, err := q.QueryContext(ctx, `SELECT id FROM contacts WHERE email IN (`+placeholders(len(chunk))+`) ORDER BY id`, args...)
		if err != nil {
			return nil, 0, fmt.Errorf("sequences: contacts by email: %w", err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, 0, err
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			return nil, 0, err
		}
		if err := rows.Err(); err != nil {
			return nil, 0, err
		}
	}
	return ids, len(emails) - len(ids), nil
}

// RegisterJobs registers the "sequences.enroll" job on Deps.Jobs. It is
// Resumable: a re-run after a restart counts contacts the interrupted run
// already enrolled instead of enrolling them again.
func (s *Service) RegisterJobs() {
	if s.jobs == nil {
		panic("sequences: RegisterJobs needs Deps.Jobs")
	}
	s.jobs.Register(JobEnroll, s.runEnrollJob, jobs.Resumable())
}

// enrollJobBatch is the number of contacts enrolled per job transaction.
const enrollJobBatch = 500

// runEnrollJob runs a sequences.enroll job in batches, each in its own
// transaction.
func (s *Service) runEnrollJob(ctx context.Context, jc jobs.JobContext) (any, error) {
	var p enrollJob
	if err := jc.Params(&p); err != nil {
		return nil, fmt.Errorf("sequences.enroll: params: %w", err)
	}
	res := &EnrollResult{Skipped: map[string]int{}}
	var done func(int64) (bool, error)
	total := len(p.Request.ContactIDs) + len(p.Request.Emails)
	processed := 0
	for _, part := range splitRequest(p.Request, enrollJobBatch) {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
			if jc.Resumed() {
				done = func(cid int64) (bool, error) {
					return enrolledSince(ctx, tx, p.SequenceID, cid, p.SubmittedAt)
				}
			}
			return s.enrollBatch(ctx, tx, p.SequenceID, part, done, res)
		})
		if err != nil {
			return res, fmt.Errorf("sequences.enroll: %w", err)
		}
		processed += len(part.ContactIDs) + len(part.Emails)
		jc.SetProgress(float64(processed) / float64(max(total, 1)))
	}
	res.EnrollmentIDs = nil
	return res, nil
}

// splitRequest splits req into requests of at most n contacts.
func splitRequest(req EnrollRequest, n int) []EnrollRequest {
	var out []EnrollRequest
	for chunk := range slices.Chunk(req.ContactIDs, n) {
		part := req
		part.ContactIDs, part.Emails = chunk, nil
		out = append(out, part)
	}
	for chunk := range slices.Chunk(req.Emails, n) {
		part := req
		part.ContactIDs, part.Emails = nil, chunk
		out = append(out, part)
	}
	return out
}

// enrolledSince reports whether the contact got an enrollment in the
// sequence at or after t.
func enrolledSince(ctx context.Context, q db.Querier, seqID, contactID int64, t time.Time) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM enrollments WHERE sequence_id = ? AND contact_id = ? AND started_at >= ?`,
		seqID, contactID, db.FormatTime(t)).Scan(&n); err != nil {
		return false, fmt.Errorf("sequences: earlier enrollment of contact %d: %w", contactID, err)
	}
	return n > 0, nil
}
