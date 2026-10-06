package broadcasts

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
)

// maxJSONBodyBytes bounds request bodies carrying an email body (1 MiB of
// content grows when JSON-escaped).
const maxJSONBodyBytes = 4 << 20

var apiTags = []string{"Broadcasts"}

// BroadcastOutput is a single broadcast with its ETag.
type BroadcastOutput struct {
	ETag string `header:"ETag" doc:"Version of the broadcast and its email; send it back in If-Match"`
	Body Broadcast
}

func broadcastOutput(b Broadcast) *BroadcastOutput {
	return &BroadcastOutput{ETag: api.ETag(b.Version), Body: b}
}

// BroadcastAudienceInput is the audience in a request.
type BroadcastAudienceInput struct {
	Type      string `json:"type" enum:"all,segment" doc:"all: every active contact; segment: the active members of segment_id" example:"segment"`
	SegmentID int64  `json:"segment_id,omitempty" minimum:"0" doc:"The segment (type segment only)" example:"3"`
}

func (a *BroadcastAudienceInput) audience() *BroadcastAudience {
	if a == nil {
		return nil
	}
	return &BroadcastAudience{Type: a.Type, SegmentID: a.SegmentID}
}

// BroadcastEmailInput is the email content of a new broadcast.
type BroadcastEmailInput struct {
	Subject   string `json:"subject" maxLength:"300" doc:"Subject line (required); may use merge fields" example:"Our autumn news, {{first_name | \"friend\"}}"`
	Preheader string `json:"preheader,omitempty" maxLength:"300" doc:"Preview text shown after the subject in the inbox" example:"Three things worth knowing"`
	Format    string `json:"format,omitempty" enum:"markdown,html" doc:"markdown (default) or html" example:"markdown"`
	Body      string `json:"body" doc:"The content, at most 1 MiB, wrapped in the base layout when sent. Merge fields: {{first_name}}, {{last_name}}, {{email}}, {{attr.<key>}}, each with an optional fallback like {{first_name | \"there\"}}" example:"Hi {{first_name | \"there\"}},\n\nHere is our news."`
}

type idInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Broadcast ID" example:"7"`
}

type listInput struct {
	api.PageParams
	Status string `query:"status" enum:"draft,scheduled,sending,paused,sent,cancelled" doc:"Only broadcasts with this status"`
}

type createInput struct {
	api.DryRunParam
	Body struct {
		Name     string                  `json:"name" maxLength:"200" doc:"Internal name" example:"October newsletter"`
		Audience *BroadcastAudienceInput `json:"audience,omitempty" doc:"Who receives it; default every active contact"`
		FromName string                  `json:"from_name,omitempty" maxLength:"200" doc:"From display name; omit for the settings default" example:"Example News"`
		ReplyTo  *string                 `json:"reply_to,omitempty" nullable:"false" doc:"Reply-To address on the sending domain; omit for the settings default, \"\" for none" example:"hello@example.com"`
		Email    BroadcastEmailInput     `json:"email" doc:"The content; the broadcast owns this email"`
	}
}

type updateInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Broadcast ID" example:"7"`
	api.IfMatchParam
	api.DryRunParam
	Body struct {
		Name              *string                 `json:"name,omitempty" nullable:"false" maxLength:"200" example:"October newsletter"`
		Audience          *BroadcastAudienceInput `json:"audience,omitempty" doc:"The new audience"`
		FromName          *string                 `json:"from_name,omitempty" nullable:"false" maxLength:"200" doc:"From display name; \"\" for the settings default" example:"Example News"`
		ReplyTo           *string                 `json:"reply_to,omitempty" nullable:"false" doc:"Reply-To on the sending domain; \"\" for none" example:"hello@example.com"`
		UseDefaultReplyTo bool                    `json:"use_default_reply_to,omitempty" doc:"true: use the settings' Reply-To again (don't combine with reply_to)"`
		Email             *struct {
			Subject   *string `json:"subject,omitempty" nullable:"false" maxLength:"300"`
			Preheader *string `json:"preheader,omitempty" nullable:"false" maxLength:"300"`
			Format    *string `json:"format,omitempty" nullable:"false" enum:"markdown,html"`
			Body      *string `json:"body,omitempty" nullable:"false"`
		} `json:"email,omitempty" doc:"Email fields to change"`
	}
}

type deleteInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Broadcast ID" example:"7"`
	api.IfMatchParam
}

type noContent struct{}

// RegisterAPI registers the Broadcasts operations.
func (s *Service) RegisterAPI(a *api.API) {
	s.registerCRUD(a)
	s.registerActions(a)
	s.registerStats(a)
}

func (s *Service) registerCRUD(a *api.API) {
	api.Register(a, huma.Operation{
		OperationID: "list-broadcasts", Method: http.MethodGet, Path: "/broadcasts", Tags: apiTags,
		Summary:     "List broadcasts",
		Description: "Returns broadcasts newest first, with their audience, status and email subject (no bodies). Filter with `status`; paginate with `limit` and `cursor`.",
	}, func(ctx context.Context, in *listInput) (*api.ListOutput[Broadcast], error) {
		before, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		bs, err := s.List(ctx, ListFilter{Status: in.Status, BeforeID: before, Limit: limit + 1})
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Broadcast]{Body: api.NewList(bs, limit, func(b Broadcast) int64 { return b.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "create-broadcast", Method: http.MethodPost, Path: "/broadcasts", Tags: apiTags,
		DefaultStatus: http.StatusCreated, MaxBodyBytes: maxJSONBodyBytes,
		Summary: "Create a broadcast",
		Description: "Creates a draft broadcast with its own email (B-1). `audience` is `{\"type\": \"all\"}` (default: every active contact) or `{\"type\": \"segment\", \"segment_id\": N}`. " +
			"`from_name` and `reply_to` override the settings; reply_to must be on the sending domain. The email content is validated like create-email (merge fields, 1 MiB). " +
			"Next: send-broadcast-test, then schedule-broadcast or send-broadcast (try `?dry_run=true` for the recipient count).",
	}, func(ctx context.Context, in *createInput) (*BroadcastOutput, error) {
		b := in.Body
		ci := CreateInput{
			Name: b.Name, FromName: b.FromName, ReplyTo: b.ReplyTo,
			Email: EmailContent{Subject: b.Email.Subject, Preheader: b.Email.Preheader, Format: b.Email.Format, Body: b.Email.Body},
		}
		if aud := b.Audience.audience(); aud != nil {
			ci.Audience = *aud
		}
		out, err := s.Create(ctx, ci, in.DryRun)
		if err != nil {
			return nil, err
		}
		return broadcastOutput(out), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-broadcast", Method: http.MethodGet, Path: "/broadcasts/{id}", Tags: apiTags,
		Summary:     "Get a broadcast",
		Description: "Returns one broadcast with its email content, audience, status and snapshot progress. The ETag covers the broadcast and its email.",
	}, func(ctx context.Context, in *idInput) (*BroadcastOutput, error) {
		b, err := s.Get(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return broadcastOutput(b), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "update-broadcast", Method: http.MethodPatch, Path: "/broadcasts/{id}", Tags: apiTags,
		MaxBodyBytes: maxJSONBodyBytes,
		Summary:      "Update a broadcast",
		Description: "Changes the fields you send: name, audience, from_name, reply_to and the email's subject, preheader, format and body. " +
			"Only draft and scheduled broadcasts can be changed (409 otherwise), also through update-email (PATCH /emails/{id}). Send the ETag in If-Match to get 412 instead of overwriting a newer change.",
	}, func(ctx context.Context, in *updateInput) (*BroadcastOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		b := in.Body
		ui := UpdateInput{Name: b.Name, Audience: b.Audience.audience(), FromName: b.FromName, ReplyTo: b.ReplyTo, DefaultReplyTo: b.UseDefaultReplyTo}
		if b.Email != nil {
			ui.Email = &EmailContentPatch{Subject: b.Email.Subject, Preheader: b.Email.Preheader, Format: b.Email.Format, Body: b.Email.Body}
		}
		out, err := s.Update(ctx, in.ID, ui, v, in.DryRun)
		if err != nil {
			return nil, err
		}
		return broadcastOutput(out), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "delete-broadcast", Method: http.MethodDelete, Path: "/broadcasts/{id}", Tags: apiTags,
		DefaultStatus: http.StatusNoContent,
		Summary:       "Delete a broadcast",
		Description: "Deletes a draft or cancelled broadcast together with its email and its unsent messages. 409 for other statuses (cancel it first) " +
			"and for a cancelled broadcast that already sent any message: sent and partly sent broadcasts are kept for their delivery history.",
	}, func(ctx context.Context, in *deleteInput) (*noContent, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		if err := s.Delete(ctx, in.ID, v); err != nil {
			return nil, err
		}
		return &noContent{}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "duplicate-broadcast", Method: http.MethodPost, Path: "/broadcasts/{id}/duplicate", Tags: apiTags,
		DefaultStatus: http.StatusCreated,
		Summary:       "Duplicate a broadcast",
		Description:   "Creates a new draft named \"<name> (copy)\" with a copy of the email, the same audience and sender overrides (B-9). Any broadcast can be duplicated. A segment audience is checked again and gets the segment's current name; if the segment was deleted, the answer is 422 on `audience.segment_id` (create a new broadcast instead).",
	}, func(ctx context.Context, in *idInput) (*BroadcastOutput, error) {
		b, err := s.Duplicate(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return broadcastOutput(b), nil
	})
}
