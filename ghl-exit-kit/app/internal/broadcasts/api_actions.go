package broadcasts

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
)

type actionInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Broadcast ID" example:"7"`
	api.IfMatchParam
}

type scheduleInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Broadcast ID" example:"7"`
	api.IfMatchParam
	Body struct {
		At time.Time `json:"at" doc:"When to start sending (RFC 3339, in the future)" example:"2026-10-01T08:00:00Z"`
	}
}

type sendInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Broadcast ID" example:"7"`
	api.IfMatchParam
	api.DryRunParam
}

type sendOutput struct {
	Status int
	Body   BroadcastSendResult
}

type resumeInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Broadcast ID" example:"7"`
	api.IfMatchParam
	Body *struct {
		Confirm bool `json:"confirm,omitempty" doc:"Required (true) to resume a broadcast paused by the circuit breaker" example:"true"`
	}
}

type testInput struct {
	ID   int64 `path:"id" minimum:"1" doc:"Broadcast ID" example:"7"`
	Body struct {
		To        []string `json:"to" minItems:"1" maxItems:"5" doc:"1-5 addresses" example:"[\"owner@example.org\"]"`
		ContactID int64    `json:"contact_id,omitempty" minimum:"0" doc:"Fill merge fields with this contact's data; omit to use the fallbacks" example:"42"`
	}
}

type testOutput struct {
	Body BroadcastTestResult
}

func (s *Service) registerActions(a *api.API) {
	api.Register(a, huma.Operation{
		OperationID: "schedule-broadcast", Method: http.MethodPost, Path: "/broadcasts/{id}/schedule", Tags: apiTags,
		Summary:     "Schedule a broadcast",
		Description: "Moves a draft to scheduled; it starts automatically at `at` (checked every 10 s), which must be in the future. Only drafts can be scheduled (409 otherwise); to change the time, unschedule first. The settings must have a From address.",
	}, func(ctx context.Context, in *scheduleInput) (*BroadcastOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		b, err := s.Schedule(ctx, in.ID, in.Body.At, v)
		if err != nil {
			return nil, err
		}
		return broadcastOutput(b), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "unschedule-broadcast", Method: http.MethodPost, Path: "/broadcasts/{id}/unschedule", Tags: apiTags,
		Summary:     "Unschedule a broadcast",
		Description: "Moves a scheduled broadcast back to draft (409 unless it is scheduled).",
	}, func(ctx context.Context, in *actionInput) (*BroadcastOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		b, err := s.Unschedule(ctx, in.ID, v)
		if err != nil {
			return nil, err
		}
		return broadcastOutput(b), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "send-broadcast", Method: http.MethodPost, Path: "/broadcasts/{id}/send", Tags: apiTags,
		DefaultStatus: http.StatusAccepted,
		Metadata: map[string]any{api.MetaAlsoResponds: map[int]string{
			http.StatusOK: "Dry run (?dry_run=true): `recipients` and `missing_tags`; nothing was sent or queued"}},
		Summary: "Send a broadcast now",
		Description: "Starts a draft or scheduled broadcast now (409 otherwise): status becomes sending and the job broadcast.snapshot queues one message per active, unsuppressed contact of the audience (B-3); poll GET /jobs/{id} or the broadcast's snapshot_done_at. " +
			"Answers 202 with the broadcast and the job. `?dry_run=true` changes nothing and answers 200 with `recipients`, the number of messages it would queue, and `missing_tags` of the audience segment. " +
			"Contacts who become inactive before their message goes out are skipped. The broadcast becomes sent when every message is out.",
	}, func(ctx context.Context, in *sendInput) (*sendOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		res, err := s.Send(ctx, in.ID, v, in.DryRun)
		if err != nil {
			return nil, err
		}
		status := http.StatusAccepted
		if in.DryRun {
			status = http.StatusOK
		}
		return &sendOutput{Status: status, Body: res}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "pause-broadcast", Method: http.MethodPost, Path: "/broadcasts/{id}/pause", Tags: apiTags,
		Summary:     "Pause a broadcast",
		Description: "Pauses a sending broadcast (paused_reason manual; 409 unless it is sending). Queued messages wait; resume-broadcast continues, cancel-broadcast stops it for good.",
	}, func(ctx context.Context, in *actionInput) (*BroadcastOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		b, err := s.Pause(ctx, in.ID, v)
		if err != nil {
			return nil, err
		}
		return broadcastOutput(b), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "resume-broadcast", Method: http.MethodPost, Path: "/broadcasts/{id}/resume", Tags: apiTags,
		Summary: "Resume a broadcast",
		Description: "Continues a paused broadcast (409 unless it is paused). A broadcast paused by the circuit breaker (paused_reason circuit_breaker) needs `{\"confirm\": true}` (422 otherwise): " +
			"check its stats first. After that the breaker judges only the messages sent after the resume.",
	}, func(ctx context.Context, in *resumeInput) (*BroadcastOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		confirm := in.Body != nil && in.Body.Confirm
		b, err := s.Resume(ctx, in.ID, confirm, v)
		if err != nil {
			return nil, err
		}
		return broadcastOutput(b), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "cancel-broadcast", Method: http.MethodPost, Path: "/broadcasts/{id}/cancel", Tags: apiTags,
		Summary:     "Cancel a broadcast",
		Description: "Cancels a draft, scheduled or paused broadcast (409 otherwise: pause a sending broadcast first). Its queued messages are skipped with error \"cancelled\". This can't be undone; duplicate-broadcast makes a new draft from it.",
	}, func(ctx context.Context, in *actionInput) (*BroadcastOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		b, err := s.Cancel(ctx, in.ID, v)
		if err != nil {
			return nil, err
		}
		return broadcastOutput(b), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "send-broadcast-test", Method: http.MethodPost, Path: "/broadcasts/{id}/test", Tags: apiTags,
		DefaultStatus: http.StatusAccepted,
		Summary:       "Send a broadcast test",
		Description: "Queues a test of the broadcast's email to 1-5 addresses (B-7), in any status. With contact_id the merge fields use that contact's data. Test messages are skipped for suppressed addresses and for addresses whose contact is unsubscribed, bounced, complained or suppressed, carry an inactive unsubscribe link and don't count in the stats. " +
			"They are sent with the broadcast's from_name and reply_to where set, else the settings' sender.",
	}, func(ctx context.Context, in *testInput) (*testOutput, error) {
		res, err := s.SendTest(ctx, in.ID, in.Body.To, in.Body.ContactID)
		if err != nil {
			return nil, err
		}
		return &testOutput{Body: res}, nil
	})
}
