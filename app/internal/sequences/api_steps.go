package sequences

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
)

// StepOutput is a step with the ETag of its sequence.
type StepOutput struct {
	ETag string `header:"ETag" doc:"The new version of the sequence; send it back in If-Match"`
	Body SequenceStep
}

type addStepInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Sequence ID" example:"4"`
	api.IfMatchParam
	Body struct {
		EmailID      int64      `json:"email_id,omitempty" minimum:"1" doc:"Send this existing email; give this or email" example:"21"`
		Email        *StepEmail `json:"email,omitempty" doc:"Create a new email with this content for the step; give this or email_id"`
		DelaySeconds int64      `json:"delay_seconds" minimum:"0" maximum:"31536000" doc:"Delay after the previous step was queued (the first step: after enrolment)" example:"86400"`
		Position     int        `json:"position,omitempty" minimum:"1" doc:"Insert at this position (later steps move down); default: at the end" example:"2"`
	}
}

type stepIDInput struct {
	ID     int64 `path:"id" minimum:"1" doc:"Sequence ID" example:"4"`
	StepID int64 `path:"step_id" minimum:"1" doc:"Step ID" example:"11"`
}

type updateStepInput struct {
	ID     int64 `path:"id" minimum:"1" doc:"Sequence ID" example:"4"`
	StepID int64 `path:"step_id" minimum:"1" doc:"Step ID" example:"11"`
	api.IfMatchParam
	Body struct {
		DelaySeconds *int64  `json:"delay_seconds,omitempty" nullable:"false" minimum:"0" maximum:"31536000" doc:"New delay; active enrollments waiting for this step move by the difference" example:"172800"`
		EmailID      *int64  `json:"email_id,omitempty" nullable:"false" minimum:"1" doc:"Send another existing email from now on" example:"22"`
		Subject      *string `json:"subject,omitempty" nullable:"false" maxLength:"300" doc:"New subject of the step's email" example:"Two days in: your first win"`
		Preheader    *string `json:"preheader,omitempty" nullable:"false" maxLength:"300" example:""`
		Format       *string `json:"format,omitempty" nullable:"false" enum:"markdown,html" example:"markdown"`
		Body         *string `json:"body,omitempty" nullable:"false" doc:"New body of the step's email" example:"Hi {{first_name | \"there\"}}, ..."`
	}
}

type deleteStepInput struct {
	ID     int64 `path:"id" minimum:"1" doc:"Sequence ID" example:"4"`
	StepID int64 `path:"step_id" minimum:"1" doc:"Step ID" example:"11"`
	api.IfMatchParam
	OnActive string `query:"on_active" enum:"advance,exit" doc:"Required when active enrollments are waiting for this step: advance moves them to the following step (or completes them), exit ends them (reason step_deleted)"`
}

type deleteStepOutput struct {
	ETag string `header:"ETag" doc:"The new version of the sequence"`
	Body DeleteStepResult
}

type reorderStepsInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Sequence ID" example:"4"`
	api.IfMatchParam
	OnActive string `query:"on_active" enum:"keep,end" doc:"What happens to active enrollments: keep (default) keeps their next step by ID, end completes them (they get no further steps)"`
	Body     struct {
		StepIDs []int64 `json:"step_ids" minItems:"1" maxItems:"100" doc:"Every step ID of the sequence, in the new order" example:"[12,11,13]"`
	}
}

type reorderStepsOutput struct {
	ETag string `header:"ETag" doc:"The new version of the sequence"`
	Body ReorderResult
}

type testStepInput struct {
	ID     int64 `path:"id" minimum:"1" doc:"Sequence ID" example:"4"`
	StepID int64 `path:"step_id" minimum:"1" doc:"Step ID" example:"11"`
	Body   struct {
		To        []string `json:"to,omitempty" maxItems:"5" doc:"1-5 addresses; omit to send to the owner's address from the settings" example:"[\"owner@example.org\"]"`
		ContactID int64    `json:"contact_id,omitempty" minimum:"1" doc:"Fill the merge fields with this contact's data (default: the fallbacks)" example:"42"`
	}
}

type testStepOutput struct {
	Body StepTestResult
}

func (s *Service) registerSteps(a *api.API) {
	api.Register(a, huma.Operation{
		OperationID: "add-sequence-step", Method: http.MethodPost, Path: "/sequences/{id}/steps", Tags: apiTags, DefaultStatus: http.StatusCreated,
		MaxBodyBytes: stepMaxBodyBytes,
		Summary:      "Add a step to a sequence",
		Description: "Adds a step that sends an existing email (`email_id`) or a new email created from `email`, `delay_seconds` after the previous step. " +
			"By default it goes at the end, where active enrollments that haven't finished will also get it. With `position` it is inserted there and later steps move down; " +
			"enrollments already past that point don't get it. Send the sequence's ETag in If-Match; the response's ETag is its new version.",
	}, func(ctx context.Context, in *addStepInput) (*StepOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		seq, st, err := s.AddStep(ctx, in.ID, StepInput{
			EmailID: in.Body.EmailID, Email: in.Body.Email, DelaySeconds: in.Body.DelaySeconds, Position: in.Body.Position,
		}, v)
		if err != nil {
			return nil, err
		}
		return &StepOutput{ETag: api.ETag(seq.Version), Body: st}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "update-sequence-step", Method: http.MethodPatch, Path: "/sequences/{id}/steps/{step_id}", Tags: apiTags,
		MaxBodyBytes: stepMaxBodyBytes,
		Summary:      "Update a sequence step",
		Description: "Changes a step's delay, switches it to another email (`email_id`), or edits its email's subject, preheader, format and body " +
			"(through the emails service; if other steps or broadcasts use the same email they change too). Content changes apply to messages not sent yet. " +
			"A new delay also moves the due time of enrollments waiting for this step by the difference. Send the sequence's ETag in If-Match.",
	}, func(ctx context.Context, in *updateStepInput) (*StepOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		b := in.Body
		seq, st, err := s.UpdateStep(ctx, in.ID, in.StepID, StepPatch{
			DelaySeconds: b.DelaySeconds, EmailID: b.EmailID, Subject: b.Subject, Preheader: b.Preheader, Format: b.Format, Body: b.Body,
		}, v)
		if err != nil {
			return nil, err
		}
		return &StepOutput{ETag: api.ETag(seq.Version), Body: st}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "delete-sequence-step", Method: http.MethodDelete, Path: "/sequences/{id}/steps/{step_id}", Tags: apiTags,
		Summary: "Delete a sequence step",
		Description: "Deletes a step; the following steps move up. When active enrollments are waiting for this step, `on_active` is required (409 without it): " +
			"`advance` gives them the following step at the time this one was due (or completes them if it was the last), `exit` ends them (reason step_deleted). " +
			"The step's email is kept and messages already queued for it are still sent. `exit` ends enrollments like any other exit, " +
			"so a message of theirs that is still queued for an earlier step (e.g. while sending is paused) is not sent; `advance` keeps it. " +
			"Send the sequence's ETag in If-Match.",
	}, func(ctx context.Context, in *deleteStepInput) (*deleteStepOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		res, err := s.DeleteStep(ctx, in.ID, in.StepID, in.OnActive, v)
		if err != nil {
			return nil, err
		}
		return &deleteStepOutput{ETag: api.ETag(res.Sequence.Version), Body: res}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "reorder-sequence-steps", Method: http.MethodPost, Path: "/sequences/{id}/steps/reorder", Tags: apiTags,
		Summary: "Reorder sequence steps",
		Description: "Puts the steps in the order of `step_ids`, which must list every step exactly once (S-7). `on_active` decides what happens to active enrollments: " +
			"`keep` (default) keeps their next step by ID, so they get that step next and continue from its new position, skipping steps they already received " +
			"(a step is never sent twice to the same enrollment, and no delay is spent on a skipped step); " +
			"`end` completes them, so they get no further steps (messages already queued are still sent). Send the sequence's ETag in If-Match.",
	}, func(ctx context.Context, in *reorderStepsInput) (*reorderStepsOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		res, err := s.ReorderSteps(ctx, in.ID, in.Body.StepIDs, in.OnActive, v)
		if err != nil {
			return nil, err
		}
		return &reorderStepsOutput{ETag: api.ETag(res.Sequence.Version), Body: res}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "send-sequence-step-test", Method: http.MethodPost, Path: "/sequences/{id}/steps/{step_id}/test", Tags: apiTags,
		DefaultStatus: http.StatusAccepted,
		Summary:       "Send a test of a step",
		Description: "Queues a test of the step's email (S-10) to 1-5 addresses, or to the owner's address when `to` is omitted, with `contact_id`'s data or the merge-field fallbacks. " +
			"Tests have no working unsubscribe or tracking links, don't count in stats, and are skipped for suppressed addresses and for addresses whose contact is unsubscribed, bounced, complained or suppressed.",
	}, func(ctx context.Context, in *testStepInput) (*testStepOutput, error) {
		res, err := s.TestStep(ctx, in.ID, in.StepID, in.Body.To, in.Body.ContactID)
		if err != nil {
			return nil, err
		}
		return &testStepOutput{Body: res}, nil
	})
}
