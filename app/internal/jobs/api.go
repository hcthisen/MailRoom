package jobs

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
)

// Output is a single-job response.
type Output struct {
	Body Job
}

type getInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Job ID" example:"17"`
}

type listInput struct {
	api.PageParams
	Type   string `query:"type" maxLength:"64" doc:"Only jobs of this type" example:"import.contacts"`
	Status string `query:"status" enum:"queued,running,succeeded,failed" doc:"Only jobs in this status"`
}

// RegisterAPI registers GET /jobs and GET /jobs/{id}.
func (r *Runner) RegisterAPI(a *api.API) {
	api.Register(a, huma.Operation{
		OperationID: "get-job",
		Method:      http.MethodGet,
		Path:        "/jobs/{id}",
		Summary:     "Get a job",
		Description: "Returns a background job: status (queued, running, succeeded, failed), progress from 0 to 1, and its result once finished. Operations that do long work (imports, broadcast snapshots, backups, bulk tagging) answer 202 with a job; poll this until status is succeeded or failed. Jobs cut short by a restart fail with error \"interrupted\", except resumable ones (such as broadcast snapshots), which run again from the start.",
		Tags:        []string{"Jobs"},
	}, func(ctx context.Context, in *getInput) (*Output, error) {
		j, err := r.Get(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &Output{Body: j}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "list-jobs",
		Method:      http.MethodGet,
		Path:        "/jobs",
		Summary:     "List jobs",
		Description: "Returns background jobs, newest first, optionally filtered by type and status.",
		Tags:        []string{"Jobs"},
	}, func(ctx context.Context, in *listInput) (*api.ListOutput[Job], error) {
		before, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		js, err := r.List(ctx, ListFilter{Type: in.Type, Status: in.Status, BeforeID: before, Limit: limit + 1})
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Job]{Body: api.NewList(js, limit, func(j Job) int64 { return j.ID })}, nil
	})
}
