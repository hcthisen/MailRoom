package segments

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
	"mailroom/internal/core"
)

// SegmentOutput is a single segment with its ETag.
type SegmentOutput struct {
	ETag string `header:"ETag" doc:"Version of the segment; send it back in If-Match"`
	Body Segment
}

func segmentOutput(sg Segment) *SegmentOutput {
	return &SegmentOutput{ETag: api.ETag(sg.Version), Body: sg}
}

type idPath struct {
	ID int64 `path:"id" minimum:"1" doc:"Segment ID" example:"3"`
}

type createInput struct {
	api.DryRunParam
	Body struct {
		Name   string        `json:"name" minLength:"1" maxLength:"100" doc:"Unique name" example:"Customers without VIP"`
		Filter SegmentFilter `json:"filter" doc:"The filter; see the Segments guide for every field and operator"`
	}
}

type updateInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Segment ID" example:"3"`
	api.IfMatchParam
	api.DryRunParam
	Body struct {
		Name   *string        `json:"name,omitempty" nullable:"false" minLength:"1" maxLength:"100" doc:"New name" example:"Customers without VIP"`
		Filter *SegmentFilter `json:"filter,omitempty" nullable:"false" doc:"New filter; replaces the whole filter"`
	}
}

type deleteInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Segment ID" example:"3"`
	api.IfMatchParam
	api.DryRunParam
}

type previewInput struct {
	Body struct {
		Filter      SegmentFilter `json:"filter" doc:"The filter to evaluate; it is not saved"`
		SampleLimit int           `json:"sample_limit,omitempty" minimum:"0" maximum:"50" doc:"How many matching contacts to return (default 10, at most 50)" example:"10"`
	}
}

type previewOutput struct {
	Body SegmentPreview
}

type countOutput struct {
	Body SegmentCounts
}

type membersInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Segment ID" example:"3"`
	api.PageParams
	Status string `query:"status" enum:"pending,active,unsubscribed,bounced,complained,suppressed_manual" doc:"Only members with this status, e.g. active for the recipients a broadcast would get"`
}

type noContent struct{}

// filterGuide is appended to the descriptions of operations that take a filter.
const filterGuide = "\n\nA filter is `{\"match\": \"all\"|\"any\", \"rules\": [...]}`; each rule is a condition or a nested group `{\"match\": ..., \"rules\": [...]}` (groups nest up to depth 3). Conditions: " +
	"`{\"field\":\"tag\",\"op\":\"has\"|\"not_has\",\"value\":\"name\"}`, " +
	"`{\"field\":\"tags\",\"op\":\"has_all\"|\"has_any\"|\"has_none\",\"values\":[...]}`, " +
	"`{\"field\":\"status\",\"op\":\"is\"|\"is_not\",\"value\":\"active\"}`, " +
	"`{\"field\":\"email\",\"op\":\"eq\"|\"contains\"|\"ends_with\",\"value\":\"...\"}`, " +
	"`{\"field\":\"first_name\"|\"last_name\",\"op\":\"eq\"|\"contains\"|\"is_empty\"|\"not_empty\",\"value\":\"...\"}`, " +
	"`{\"field\":\"created_at\",\"op\":\"before\"|\"after\",\"value\":\"2026-01-01T00:00:00Z\"}` or `{\"field\":\"created_at\",\"op\":\"within_days\",\"days\":30}`, " +
	"`{\"field\":\"attr\",\"key\":\"plan\",\"op\":\"eq\"|\"neq\"|\"contains\"|\"exists\"|\"not_exists\",\"value\":\"pro\"}` (value: string, number or boolean; the type must match the stored one), " +
	"`{\"field\":\"clicked\",\"op\":\"any_link\"|\"link\"|\"no_click\",\"url\":\"...\",\"broadcast_id\":12,\"within_days\":30}` (url required for link, optional for no_click). " +
	"Text comparisons ignore case; `contains` and `ends_with` match literally. Tags are referenced by name; `missing_tags` in counts and previews lists names that no tag has (a has rule on one matches nobody, a not_has/has_none rule everybody). A filter never adds a status condition by itself. " +
	"Validation errors name the offending input, e.g. field `filter.rules[2].value`."

// RegisterAPI registers the Segments operations.
func (s *Service) RegisterAPI(a *api.API) {
	tags := []string{"Segments"}

	api.Register(a, huma.Operation{
		OperationID: "list-segments", Method: http.MethodGet, Path: "/segments", Tags: tags,
		Summary:     "List segments",
		Description: "Returns the saved segments with their filters, ordered by ID. Segments are evaluated when used, so the list holds no member counts; use get-segment-count for those.",
	}, func(ctx context.Context, in *api.PageParams) (*api.ListOutput[Segment], error) {
		after, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		list, err := s.List(ctx, after, limit+1)
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Segment]{Body: api.NewList(list, limit, func(sg Segment) int64 { return sg.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "create-segment", Method: http.MethodPost, Path: "/segments", Tags: tags, DefaultStatus: http.StatusCreated,
		Summary:     "Create a segment",
		Description: "Saves a named filter. Names are unique, ignoring case (409 on a duplicate). With `?dry_run=true` the segment is validated and returned but not saved." + filterGuide,
	}, func(ctx context.Context, in *createInput) (*SegmentOutput, error) {
		sg, err := s.Create(ctx, CreateInput{Name: in.Body.Name, Filter: in.Body.Filter}, in.DryRun)
		if err != nil {
			return nil, err
		}
		return segmentOutput(sg), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "preview-segment", Method: http.MethodPost, Path: "/segments/preview", Tags: tags,
		Metadata:    map[string]any{api.MetaScope: api.ScopeRead},
		Summary:     "Preview a filter",
		Description: "Evaluates a filter without saving it and returns the live counts (total and active) plus a sample of the most recently created matching contacts. This is the segment builder's live count (T-3). It changes nothing, so a read token may call it." + filterGuide,
	}, func(ctx context.Context, in *previewInput) (*previewOutput, error) {
		p, err := s.Preview(ctx, in.Body.Filter, in.Body.SampleLimit)
		if err != nil {
			return nil, err
		}
		return &previewOutput{Body: p}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-segment", Method: http.MethodGet, Path: "/segments/{id}", Tags: tags,
		Summary:     "Get a segment",
		Description: "Returns one segment with its filter. The ETag header carries its version for If-Match.",
	}, func(ctx context.Context, in *idPath) (*SegmentOutput, error) {
		sg, err := s.Get(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return segmentOutput(sg), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "update-segment", Method: http.MethodPatch, Path: "/segments/{id}", Tags: tags,
		Summary:     "Update a segment",
		Description: "Renames a segment and/or replaces its filter; omitted members stay unchanged. Broadcasts that already took their recipient snapshot are not affected. Send the ETag in If-Match to get 412 instead of overwriting a concurrent change. With `?dry_run=true` the change is validated and returned but not saved." + filterGuide,
	}, func(ctx context.Context, in *updateInput) (*SegmentOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		sg, err := s.Update(ctx, in.ID, UpdateInput{Name: in.Body.Name, Filter: in.Body.Filter}, v, in.DryRun)
		if err != nil {
			return nil, err
		}
		return segmentOutput(sg), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "delete-segment", Method: http.MethodDelete, Path: "/segments/{id}", Tags: tags, DefaultStatus: http.StatusNoContent,
		Summary:     "Delete a segment",
		Description: "Deletes a segment. It is refused with 409 while a broadcast in draft, scheduled, sending or paused uses it as its audience. Send the ETag in If-Match to get 412 if it changed. With `?dry_run=true` the checks run but nothing is deleted.",
	}, func(ctx context.Context, in *deleteInput) (*noContent, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		return &noContent{}, s.Delete(ctx, in.ID, v, in.DryRun)
	})

	api.Register(a, huma.Operation{
		OperationID: "get-segment-count", Method: http.MethodGet, Path: "/segments/{id}/count", Tags: tags,
		Summary:     "Count a segment",
		Description: "Evaluates the segment now and returns how many contacts match (total) and how many of them are active, i.e. would receive a broadcast to it. `missing_tags` lists tag names the filter refers to that no tag has, e.g. after a tag was deleted.",
	}, func(ctx context.Context, in *idPath) (*countOutput, error) {
		c, err := s.CountSegment(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &countOutput{Body: c}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "list-segment-members", Method: http.MethodGet, Path: "/segments/{id}/members", Tags: tags,
		Summary:     "List a segment's members",
		Description: "Evaluates the segment now and returns the matching contacts, ordered by ID, with cursor pagination. Filter by status, e.g. `status=active` for a broadcast's would-be recipients.",
	}, func(ctx context.Context, in *membersInput) (*api.ListOutput[SegmentMember], error) {
		after, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		list, err := s.SegmentMembers(ctx, in.ID, MemberQuery{AfterID: after, Limit: limit + 1, Status: core.ContactStatus(in.Status)})
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[SegmentMember]{Body: api.NewList(list, limit, func(m SegmentMember) int64 { return m.ID })}, nil
	})
}
