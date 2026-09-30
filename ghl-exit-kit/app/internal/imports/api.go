package imports

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"mailroom/internal/api"
)

// maxRequestBytes bounds a whole import request: the file plus multipart
// or JSON overhead (JSON escapes can grow the text a little).
const maxRequestBytes = MaxFileBytes + MaxFileBytes/8 + 64<<10

// operation IDs.
const (
	opCreate = "create-import"
	opGet    = "get-import"
	opList   = "list-imports"
)

// requestKey carries the *http.Request of create-import to its handler,
// which reads the body itself (multipart, JSON or CSV).
type requestKey struct{}

// uploadTimeout bounds reading an import request and writing its answer.
// The server's ReadTimeout (2 minutes) covers the whole body, which is too
// short for a 50 MiB file on a slow uplink (about 3.5 Mbit/s and less), so
// create-import extends both deadlines for its own connection. It runs
// after authentication: only a token or session holder gets the longer
// deadline.
const uploadTimeout = 30 * time.Minute

// captureRequest limits the request body, extends the connection's read
// and write deadlines for the upload, and hands the request to the
// handler. Huma doesn't read the body: the operation declares none, so it
// never applies a body read timeout of its own.
func captureRequest(ctx huma.Context, next func(huma.Context)) {
	r, w := humago.Unwrap(ctx)
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(uploadTimeout)
	// Writers without deadlines (httptest) answer ErrNotSupported; the
	// server's own timeouts then stay in force, which is all we can do.
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	next(huma.WithValue(ctx, requestKey{}, r))
}

type createImportInput struct {
	api.DryRunParam
	OnExisting string `query:"on_existing" enum:"update,skip" doc:"What to do with contacts that already exist: update (default) fills in names and attributes and adds tags; skip leaves them as they are. Either way a status that blocks sending (unsubscribed, bounced, complained) is applied and no contact is ever reactivated. May also be sent as the on_existing form or JSON field."`
}

type createImportOutput struct {
	Status int
	Body   ImportResult
}

type importIDInput struct {
	JobID int64 `path:"job_id" minimum:"1" doc:"The import's job ID" example:"17"`
}

type importOutput struct {
	Body ImportJob
}

// RegisterAPI registers the Imports operations.
func (s *Service) RegisterAPI(a *api.API) {
	tags := []string{"Imports"}

	api.Register(a, huma.Operation{
		OperationID: opCreate, Method: http.MethodPost, Path: "/imports", Tags: tags, DefaultStatus: http.StatusAccepted,
		Middlewares: huma.Middlewares{captureRequest},
		Summary:     "Import contacts from CSV",
		Description: "Imports contacts from a CSV file (I-1; format in docs/import-format.md): a header row with `email` (required), `first_name`, `last_name`, " +
			"`status` (active, unsubscribed, bounced, complained), `tags` (semicolon-separated), `consent_source`, `consent_date` (RFC 3339 or YYYY-MM-DD) and `attr.<key>` columns. " +
			"Send it as multipart/form-data (field `file`), as JSON `{\"csv\": \"...\"}`, or as a text/csv body; at most 50 MiB. " +
			"Answers 202 with a job (type import.contacts); poll get-import for progress and the report. " +
			"Every imported contact gets a consent event with source `import:<consent_source>` (I-2). Erased and suppressed addresses are rejected, " +
			"and an import never reactivates a contact: a status only moves away from active. " +
			"`?dry_run=true` evaluates the whole file now, writes nothing and answers 200 with `report`. " +
			"A file without a header row or email column is 422 with field `file`.",
	}, s.handleCreate)

	api.Register(a, huma.Operation{
		OperationID: opGet, Method: http.MethodGet, Path: "/imports/{job_id}", Tags: tags,
		Summary:     "Get an import and its report",
		Description: "Returns an import's status and progress and, once it has finished, its report: rows, created, updated, unchanged, skipped, duplicates_merged, not_reactivated, status_kept, rejected rows (line numbers and reasons, never addresses), status and tag counts, and warnings.",
	}, func(ctx context.Context, in *importIDInput) (*importOutput, error) {
		j, err := s.Get(ctx, in.JobID)
		if err != nil {
			return nil, err
		}
		return &importOutput{Body: j}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: opList, Method: http.MethodGet, Path: "/imports", Tags: tags,
		Summary:     "List imports",
		Description: "Returns imports newest first, with status and progress. Use get-import for an import's report.",
	}, func(ctx context.Context, in *api.PageParams) (*api.ListOutput[ImportJob], error) {
		before, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		js, err := s.List(ctx, before, limit+1)
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[ImportJob]{Body: api.NewList(js, limit, func(j ImportJob) int64 { return j.JobID })}, nil
	})

	documentUploadBody(a.OpenAPI(), opCreate)
}

func (s *Service) handleCreate(ctx context.Context, in *createImportInput) (*createImportOutput, error) {
	r, _ := ctx.Value(requestKey{}).(*http.Request)
	if r == nil {
		return nil, errors.New("imports: request not captured")
	}
	if _, err := validOnExisting(in.OnExisting); err != nil {
		return nil, err
	}
	up, err := s.readUpload(ctx, r, in.DryRun, in.OnExisting)
	if err != nil {
		return nil, uploadErr(err)
	}
	if in.DryRun {
		rep, err := s.evaluate(ctx, up.parsed, up.onExisting)
		if err != nil {
			return nil, err
		}
		return &createImportOutput{Status: http.StatusOK, Body: ImportResult{Report: &rep}}, nil
	}
	j, err := s.start(ctx, up.stored, up.size, Options{OnExisting: up.onExisting, Filename: up.filename})
	if err != nil {
		return nil, err
	}
	return &createImportOutput{Status: http.StatusAccepted, Body: ImportResult{Job: &j}}, nil
}

// uploadErr maps a body that exceeds the request limit to 413 and an
// upload that outlasts uploadTimeout to 408, instead of a 500.
func uploadErr(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return api.NewError(http.StatusRequestEntityTooLarge, api.CodeValidation,
			fmt.Sprintf("the request is larger than the %d MiB limit", MaxFileBytes>>20), "file")
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return api.NewError(http.StatusRequestTimeout, api.CodeValidation,
			fmt.Sprintf("the upload took longer than %d minutes; split the file or use a faster connection", int(uploadTimeout/time.Minute)), "file")
	}
	return err
}

// documentUploadBody documents create-import's request body, which the
// handler reads itself, and its dry-run answer (200, same body schema as
// the 202) in the OpenAPI spec.
func documentUploadBody(o *huma.OpenAPI, opID string) {
	onExisting := &huma.Schema{Type: "string", Enum: []any{OnExistingUpdate, OnExistingSkip},
		Description: "Same as the on_existing query parameter"}
	body := &huma.RequestBody{
		Required:    true,
		Description: "The CSV file, at most 50 MiB",
		Content: map[string]*huma.MediaType{
			"multipart/form-data": {Schema: &huma.Schema{
				Type:     "object",
				Required: []string{"file"},
				Properties: map[string]*huma.Schema{
					"file":        {Type: "string", Format: "binary", Description: "The CSV file"},
					"on_existing": onExisting,
				},
			}},
			"application/json": {Schema: &huma.Schema{
				Type:     "object",
				Required: []string{"csv"},
				Properties: map[string]*huma.Schema{
					"csv":         {Type: "string", Description: "The CSV text", Examples: []any{exampleCSV}},
					"on_existing": onExisting,
				},
				AdditionalProperties: false,
			}},
			"text/csv": {Schema: &huma.Schema{Type: "string", Description: "The CSV text", Examples: []any{exampleCSV}}},
		},
	}
	for _, item := range o.Paths {
		op := item.Post
		if op == nil || op.OperationID != opID {
			continue
		}
		op.RequestBody = body
		if accepted := op.Responses[strconv.Itoa(http.StatusAccepted)]; accepted != nil {
			op.Responses[strconv.Itoa(http.StatusOK)] = &huma.Response{
				Description: "Dry run (?dry_run=true): the report of what the import would do, in `report`; nothing was written",
				Content:     accepted.Content,
			}
		}
	}
}

// exampleCSV is the request example in the spec.
const exampleCSV = "email,first_name,last_name,status,tags,consent_source,consent_date,attr.company\n" +
	"alice@example.com,Alice,Smith,active,newsletter;customer,ghl-form,2024-03-01,Acme\n" +
	"bob@example.com,Bob,,unsubscribed,newsletter,ghl-import,2023-11-20T09:30:00Z,\n"
