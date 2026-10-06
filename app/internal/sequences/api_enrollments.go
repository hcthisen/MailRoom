package sequences

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
)

type listEnrollmentsInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Sequence ID" example:"4"`
	api.PageParams
	Status string `query:"status" enum:"active,completed,exited" doc:"Only enrollments with this status"`
}

type enrollInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Sequence ID" example:"4"`
	api.DryRunParam
	Body EnrollRequest
}

type enrollOutput struct {
	Status int
	Body   EnrollResult
}

type enrollmentIDInput struct {
	ID           int64 `path:"id" minimum:"1" doc:"Sequence ID" example:"4"`
	EnrollmentID int64 `path:"enrollment_id" minimum:"1" doc:"Enrollment ID" example:"301"`
}

type enrollmentOutput struct {
	Body SequenceEnrollment
}

func (s *Service) registerEnrollments(a *api.API) {
	api.Register(a, huma.Operation{
		OperationID: "list-sequence-enrollments", Method: http.MethodGet, Path: "/sequences/{id}/enrollments", Tags: apiTags,
		Summary:     "List a sequence's enrollments",
		Description: "Returns the sequence's enrollments newest first, with the contact's email, status, next step and when it is due. Filter with `status`; paginate with `limit` and `cursor`.",
	}, func(ctx context.Context, in *listEnrollmentsInput) (*api.ListOutput[SequenceEnrollment], error) {
		before, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		list, err := s.ListEnrollments(ctx, in.ID, EnrollmentFilter{Status: in.Status, BeforeID: before, Limit: limit + 1})
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[SequenceEnrollment]{Body: api.NewList(list, limit, func(e SequenceEnrollment) int64 { return e.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "enroll-sequence-contacts", Method: http.MethodPost, Path: "/sequences/{id}/enrollments", Tags: apiTags,
		Metadata: map[string]any{api.MetaAlsoResponds: map[int]string{
			http.StatusAccepted: "More than 500 contacts: `job` is the enrolment job (type sequences.enroll); its result has the other fields"}},
		MaxBodyBytes: enrollMaxBodyBytes,
		Summary:      "Enrol contacts in a sequence",
		Description: "Enrols contacts given by `contact_ids` or `emails` (S-2, S-8). Only active contacts are enrolled, and the sequence's re-entry policy applies; " +
			"`skipped` counts the others by reason (not_found, not_active, already_active, reentry, exit_tag, no_steps, unsubscribed). " +
			"For contacts migrated mid-drip, `at_step` starts at that step position and `start_at` sets when (that step is due at start_at plus its delay). " +
			"With `dry_run=true` nothing changes and `would_enroll` says how many would be enrolled. More than 500 contacts are enrolled by a job: " +
			"the response is 202 with `job`, and the job's result has the same fields.",
	}, func(ctx context.Context, in *enrollInput) (*enrollOutput, error) {
		res, err := s.EnrollContacts(ctx, in.ID, in.Body, in.DryRun)
		if err != nil {
			return nil, err
		}
		out := &enrollOutput{Status: http.StatusOK, Body: res}
		if res.Job != nil {
			out.Status = http.StatusAccepted
		}
		return out, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "unenroll-sequence-contact", Method: http.MethodDelete, Path: "/sequences/{id}/enrollments/{enrollment_id}", Tags: apiTags,
		Summary:     "End an enrollment",
		Description: "Ends an active enrollment manually (reason manual); the contact gets no further steps and queued ones are skipped. A finished enrollment is 409.",
	}, func(ctx context.Context, in *enrollmentIDInput) (*enrollmentOutput, error) {
		e, err := s.Unenroll(ctx, in.ID, in.EnrollmentID)
		if err != nil {
			return nil, err
		}
		return &enrollmentOutput{Body: e}, nil
	})
}
