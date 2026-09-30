package contacts

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
)

type tagIDInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Tag ID" example:"3"`
}

type tagOutput struct {
	Body Tag
}

type listTagsInput struct {
	api.PageParams
	Q string `query:"q" maxLength:"64" doc:"Only tags whose name contains this (case-insensitive)" example:"cust"`
}

type createTagInput struct {
	Body struct {
		Name string `json:"name" minLength:"1" maxLength:"64" doc:"Tag name, unique case-insensitively" example:"customer"`
	}
}

type renameTagInput struct {
	ID   int64 `path:"id" minimum:"1" doc:"Tag ID" example:"3"`
	Body struct {
		Name string `json:"name" minLength:"1" maxLength:"64" doc:"The new name" example:"customers"`
	}
}

type mergeTagInput struct {
	ID   int64 `path:"id" minimum:"1" doc:"ID of the tag to merge away" example:"4"`
	Body struct {
		IntoTagID int64 `json:"into_tag_id" minimum:"1" doc:"ID of the tag that remains" example:"3"`
		Force     bool  `json:"force,omitempty" doc:"Merge even when actively enrolled contacts would gain an exit tag of their sequence (they leave it before the next step)"`
	}
}

type mergeTagOutput struct {
	Body MergeResult
}

type bulkTagInput struct {
	api.DryRunParam
	Body BulkTagInput
}

type bulkTagOutput struct {
	Status int
	Body   BulkTagResult
}

func (s *Service) registerTags(a *api.API) {
	tags := []string{"Tags"}

	api.Register(a, huma.Operation{
		OperationID: "list-tags", Method: http.MethodGet, Path: "/tags", Tags: tags,
		Summary:     "List tags",
		Description: "Returns tags in creation order with the number of contacts that have each.",
	}, func(ctx context.Context, in *listTagsInput) (*api.ListOutput[Tag], error) {
		after, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		ts, err := s.ListTags(ctx, in.Q, after, limit+1)
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Tag]{Body: api.NewList(ts, limit, func(t Tag) int64 { return t.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-tag", Method: http.MethodGet, Path: "/tags/{id}", Tags: tags,
		Summary:     "Get a tag",
		Description: "Returns one tag with its contact count.",
	}, func(ctx context.Context, in *tagIDInput) (*tagOutput, error) {
		t, err := s.GetTag(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &tagOutput{Body: t}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "create-tag", Method: http.MethodPost, Path: "/tags", Tags: tags, DefaultStatus: http.StatusCreated,
		Summary:     "Create a tag",
		Description: "Creates an empty tag, e.g. to use as a sequence trigger before any contact has it. Names are unique case-insensitively (409 if taken). Adding a tag to a contact also creates it.",
	}, func(ctx context.Context, in *createTagInput) (*tagOutput, error) {
		t, err := s.CreateTag(ctx, in.Body.Name)
		if err != nil {
			return nil, err
		}
		return &tagOutput{Body: t}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "rename-tag", Method: http.MethodPatch, Path: "/tags/{id}", Tags: tags,
		Summary:     "Rename a tag",
		Description: "Renames a tag. Contacts, sequences and forms keep using it, and saved segments that select on the old name are rewritten to the new one. 409 if another tag already has the name (merge them instead).",
	}, func(ctx context.Context, in *renameTagInput) (*tagOutput, error) {
		t, err := s.RenameTag(ctx, in.ID, in.Body.Name)
		if err != nil {
			return nil, err
		}
		return &tagOutput{Body: t}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "merge-tag", Method: http.MethodPost, Path: "/tags/{id}/merge", Tags: tags,
		Summary: "Merge a tag into another",
		Description: "Moves every contact of tag `id` to `into_tag_id`, repoints sequences and forms that used it, rewrites saved segments to select on the target name, and deletes tag `id`. No sequence triggers fire for the moved memberships. " +
			"409 when a sequence's trigger tag would become one of its own exit tags, and (unless `force` is true) when actively enrolled contacts would gain an exit tag of their sequence; the message names the sequence.",
	}, func(ctx context.Context, in *mergeTagInput) (*mergeTagOutput, error) {
		res, err := s.MergeTag(ctx, in.ID, in.Body.IntoTagID, in.Body.Force)
		if err != nil {
			return nil, err
		}
		return &mergeTagOutput{Body: res}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "delete-tag", Method: http.MethodDelete, Path: "/tags/{id}", Tags: tags, DefaultStatus: http.StatusNoContent,
		Summary:     "Delete a tag",
		Description: "Deletes a tag and removes it from every contact. A tag used by a sequence (trigger or exit tag), a form or a saved segment can't be deleted (409): change those first, or merge the tag into another.",
	}, func(ctx context.Context, in *tagIDInput) (*noContent, error) {
		return &noContent{}, s.DeleteTag(ctx, in.ID)
	})

	api.Register(a, huma.Operation{
		OperationID: "bulk-tag", Method: http.MethodPost, Path: "/tags/bulk", Tags: tags, DefaultStatus: http.StatusAccepted,
		Metadata: map[string]any{api.MetaAlsoResponds: map[int]string{
			http.StatusOK: "Dry run (?dry_run=true): only `matched`, the number of contacts matched now; no job was created"}},
		Summary: "Add or remove a tag in bulk",
		Description: "Adds `tag` to (or removes it from) the contacts in `contact_ids` or in segment `segment_id` (evaluated when the job runs). " +
			"Answers 202 with a job (type tags.bulk); poll get-job for progress and the result (changed, unchanged, missing, failed). " +
			"Adding fires tag triggers like single adds do. `?dry_run=true` answers 200 with only `matched`, the number of contacts matched now.",
	}, func(ctx context.Context, in *bulkTagInput) (*bulkTagOutput, error) {
		res, err := s.BulkTag(ctx, in.Body, in.DryRun)
		if err != nil {
			return nil, err
		}
		status := http.StatusAccepted
		if in.DryRun {
			status = http.StatusOK
		}
		return &bulkTagOutput{Status: status, Body: res}, nil
	})
}
