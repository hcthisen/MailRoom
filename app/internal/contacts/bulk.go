package contacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"mailroom/internal/api"
	"mailroom/internal/core"
	"mailroom/internal/jobs"
)

// JobBulkTag is the job type of bulk tagging.
const JobBulkTag = "tags.bulk"

// Bulk tag actions.
const (
	BulkAdd    = "add"
	BulkRemove = "remove"
)

// bulkBatchSize is the number of contacts tagged per transaction.
const bulkBatchSize = 500

// MaxBulkContactIDs bounds the contact_ids of one bulk request.
const MaxBulkContactIDs = 50_000

// maxJobErrors bounds the error messages kept in a job result.
const maxJobErrors = 20

// BulkTagInput adds a tag to, or removes it from, a set of contacts given
// either as IDs or as a segment (evaluated when the job runs).
type BulkTagInput struct {
	Tag        string  `json:"tag" minLength:"1" maxLength:"64" doc:"The tag name" example:"customer"`
	Action     string  `json:"action" enum:"add,remove" doc:"add or remove the tag" example:"add"`
	ContactIDs []int64 `json:"contact_ids,omitempty" maxItems:"50000" doc:"The contacts to change; give this or segment_id" example:"[12,15]"`
	SegmentID  int64   `json:"segment_id,omitempty" minimum:"1" doc:"Change every contact in this segment; give this or contact_ids" example:"3"`
}

// validate normalises in and checks it.
func (in *BulkTagInput) validate() error {
	tag, err := normalizeTagField(in.Tag, "tag")
	if err != nil {
		return err
	}
	in.Tag = tag
	if in.Action != BulkAdd && in.Action != BulkRemove {
		return core.Invalid("action", "must be add or remove")
	}
	if (len(in.ContactIDs) > 0) == (in.SegmentID > 0) {
		return core.Invalid("contact_ids", "give either contact_ids or segment_id")
	}
	if len(in.ContactIDs) > MaxBulkContactIDs {
		return core.Invalid("contact_ids", "at most %d contact IDs are allowed", MaxBulkContactIDs)
	}
	ids := slices.Clone(in.ContactIDs)
	slices.Sort(ids)
	in.ContactIDs = slices.Compact(ids)
	if len(in.ContactIDs) > 0 && in.ContactIDs[0] <= 0 {
		return core.Invalid("contact_ids", "IDs must be positive")
	}
	return nil
}

// BulkTagResult is a bulk request's outcome: the match count for a dry
// run, otherwise the submitted job.
type BulkTagResult struct {
	Matched int       `json:"matched" doc:"Contacts matched now (existing contacts, or current segment members)" example:"250"`
	Job     *jobs.Job `json:"job,omitempty" doc:"The job doing the work; absent in a dry run"`
}

// BulkTag validates the request and counts the matching contacts. With
// dryRun it only returns the count; otherwise it submits a tags.bulk job
// in the same transaction as its audit entry.
func (s *Service) BulkTag(ctx context.Context, in BulkTagInput, dryRun bool) (BulkTagResult, error) {
	if err := in.validate(); err != nil {
		return BulkTagResult{}, err
	}
	var res BulkTagResult
	err := api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		n, err := s.countBulk(ctx, tx, in)
		if err != nil {
			return err
		}
		res.Matched = n
		if dryRun {
			return nil
		}
		if s.jobs == nil {
			return errors.New("contacts: no job runner configured")
		}
		j, err := s.jobs.Submit(ctx, tx, JobBulkTag, in)
		if err != nil {
			return err
		}
		res.Job = &j
		return s.logAudit(ctx, tx, "tag.bulk", "job", j.ID, map[string]any{
			"tag": in.Tag, "action": in.Action, "segment_id": in.SegmentID, "contacts": len(in.ContactIDs), "matched": n,
		})
	})
	return res, err
}

// countBulk counts the contacts the request matches now.
func (s *Service) countBulk(ctx context.Context, q *sql.Tx, in BulkTagInput) (int, error) {
	var n int
	if in.SegmentID > 0 {
		expr, args, err := s.segmentWhere(ctx, in.SegmentID)
		if err != nil {
			return 0, err
		}
		err = q.QueryRowContext(ctx, `SELECT count(*) FROM contacts c WHERE `+expr, args...).Scan(&n)
		if err != nil {
			return 0, fmt.Errorf("count segment %d: %w", in.SegmentID, err)
		}
		return n, nil
	}
	for chunk := range slices.Chunk(in.ContactIDs, bulkBatchSize) {
		var c int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM contacts WHERE id IN (`+placeholders(len(chunk))+`)`,
			int64sToAny(chunk)...).Scan(&c); err != nil {
			return 0, fmt.Errorf("count contacts: %w", err)
		}
		n += c
	}
	return n, nil
}

// BulkTagJobResult is the result of a tags.bulk job.
type BulkTagJobResult struct {
	Tag       string   `json:"tag"`
	Action    string   `json:"action"`
	Matched   int      `json:"matched" doc:"Contacts processed"`
	Changed   int      `json:"changed" doc:"Contacts that gained or lost the tag"`
	Unchanged int      `json:"unchanged" doc:"Contacts that already had (add) or didn't have (remove) the tag"`
	Missing   int      `json:"missing" doc:"Requested contact IDs that don't exist"`
	Failed    int      `json:"failed" doc:"Contacts that could not be changed (see errors)"`
	Errors    []string `json:"errors" doc:"The first errors, one per failed contact"`
}

// RegisterJobs registers the tags.bulk job handler. It is Resumable: adding
// or removing a tag is idempotent, so an interrupted job simply runs again.
func (s *Service) RegisterJobs(r *jobs.Runner) {
	r.Register(JobBulkTag, s.runBulkTag, jobs.Resumable())
}

// runBulkTag processes a tags.bulk job in batches of bulkBatchSize, each
// in its own transaction, through contactstore so TagAdded hooks fire.
func (s *Service) runBulkTag(ctx context.Context, jc jobs.JobContext) (any, error) {
	var in BulkTagInput
	if err := jc.Params(&in); err != nil {
		return nil, fmt.Errorf("tags.bulk: params: %w", err)
	}
	if err := in.validate(); err != nil {
		return nil, fmt.Errorf("tags.bulk: %w", err)
	}
	res := &BulkTagJobResult{Tag: in.Tag, Action: in.Action, Errors: []string{}}
	if in.SegmentID > 0 {
		return res, s.bulkSegment(ctx, jc, in, res)
	}
	return res, s.bulkIDs(ctx, jc, in, res)
}

// bulkIDs processes explicit contact IDs.
func (s *Service) bulkIDs(ctx context.Context, jc jobs.JobContext, in BulkTagInput, res *BulkTagJobResult) error {
	total := len(in.ContactIDs)
	done := 0
	for chunk := range slices.Chunk(in.ContactIDs, bulkBatchSize) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.bulkBatch(ctx, in, chunk, res); err != nil {
			return err
		}
		done += len(chunk)
		jc.SetProgress(float64(done) / float64(total))
	}
	return nil
}

// bulkSegment processes the segment's current members in ID order.
func (s *Service) bulkSegment(ctx context.Context, jc jobs.JobContext, in BulkTagInput, res *BulkTagJobResult) error {
	expr, args, err := s.segmentWhere(ctx, in.SegmentID)
	if err != nil {
		return err
	}
	var total int
	if err := s.db.Read.QueryRowContext(ctx, `SELECT count(*) FROM contacts c WHERE `+expr, args...).Scan(&total); err != nil {
		return fmt.Errorf("tags.bulk: count segment: %w", err)
	}
	var after int64
	done := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ids, err := s.segmentPage(ctx, expr, args, after)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		if err := s.bulkBatch(ctx, in, ids, res); err != nil {
			return err
		}
		after = ids[len(ids)-1]
		done += len(ids)
		if total > 0 {
			jc.SetProgress(min(1, float64(done)/float64(total)))
		}
	}
	return nil
}

// segmentPage returns the next bulkBatchSize segment member IDs after after.
func (s *Service) segmentPage(ctx context.Context, expr string, args []any, after int64) ([]int64, error) {
	q := `SELECT c.id FROM contacts c WHERE (` + expr + `) AND c.id > ? ORDER BY c.id LIMIT ?`
	rows, err := s.db.Read.QueryContext(ctx, q, append(slices.Clone(args), after, bulkBatchSize)...)
	if err != nil {
		return nil, fmt.Errorf("tags.bulk: segment members: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// bulkBatch changes one batch of contacts in one transaction. A contact
// that fails (a hook error, say) is counted and skipped; contactstore
// leaves nothing of it in the transaction.
func (s *Service) bulkBatch(ctx context.Context, in BulkTagInput, ids []int64, res *BulkTagJobResult) error {
	var batch BulkTagJobResult
	var errs []string
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		batch, errs = BulkTagJobResult{}, nil
		state, err := tagState(ctx, tx, ids, in.Tag)
		if err != nil {
			return err
		}
		source := changeSource(ctx)
		for _, id := range ids {
			has, exists := state[id]
			if !exists {
				batch.Missing++
				continue
			}
			batch.Matched++
			if has == (in.Action == BulkAdd) {
				batch.Unchanged++
				continue
			}
			var err error
			if in.Action == BulkAdd {
				err = s.store.AddTags(ctx, tx, id, []string{in.Tag}, source)
			} else {
				err = s.store.RemoveTags(ctx, tx, id, []string{in.Tag})
			}
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				batch.Failed++
				errs = append(errs, fmt.Sprintf("contact %d: %v", id, err))
				continue
			}
			batch.Changed++
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("tags.bulk: %w", err)
	}
	res.Matched += batch.Matched
	res.Changed += batch.Changed
	res.Unchanged += batch.Unchanged
	res.Missing += batch.Missing
	res.Failed += batch.Failed
	for _, e := range errs {
		if len(res.Errors) < maxJobErrors {
			res.Errors = append(res.Errors, e)
		}
	}
	return nil
}

// tagState reports, for each existing contact in ids, whether it has tag.
func tagState(ctx context.Context, tx *sql.Tx, ids []int64, tag string) (map[int64]bool, error) {
	args := append(int64sToAny(ids), tag)
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, EXISTS (SELECT 1 FROM contact_tags ct JOIN tags t ON t.id = ct.tag_id
			WHERE ct.contact_id = c.id AND t.name = ?`+fmt.Sprint(len(ids)+1)+`)
		FROM contacts c WHERE c.id IN (`+numbered(len(ids))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read tag state: %w", err)
	}
	defer rows.Close()
	state := make(map[int64]bool, len(ids))
	for rows.Next() {
		var id int64
		var has bool
		if err := rows.Scan(&id, &has); err != nil {
			return nil, err
		}
		state[id] = has
	}
	return state, rows.Err()
}

// numbered returns "?1, ?2, ..., ?n".
func numbered(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "?%d", i)
	}
	return b.String()
}

func int64sToAny(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
