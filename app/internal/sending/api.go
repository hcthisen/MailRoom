package sending

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
)

// Status is the body of the sending state operations: the global pause
// state plus the current send rate and the last SES account snapshot.
type Status struct {
	State
	RatePerSec float64  `json:"rate_per_sec" doc:"Current global send rate in messages per second (B-4)" example:"11.2"`
	SESAccount *Account `json:"ses_account,omitempty" doc:"Last SES account snapshot (quota, sending enabled); absent until first polled"`
}

// StatusOutput is a sending state response.
type StatusOutput struct {
	Body Status
}

type stateInput struct{}

type pauseInput struct {
	api.DryRunParam
	Body *struct {
		Reason string `json:"reason,omitempty" maxLength:"500" doc:"Why sending is paused; shown in the state and the audit log" example:"Checking the list before the next batch"`
	} `required:"false"`
}

type resumeInput struct {
	api.DryRunParam
}

type listMessagesInput struct {
	api.PageParams
	Kind         string `query:"kind" enum:"broadcast,sequence,optin,test,alert" doc:"Only messages of this kind"`
	Status       string `query:"status" enum:"queued,sending,sent,delivered,bounced,complained,failed,skipped" doc:"Only messages in this status"`
	BroadcastID  int64  `query:"broadcast_id" minimum:"1" doc:"Only messages of this broadcast"`
	ContactID    int64  `query:"contact_id" minimum:"1" doc:"Only messages to this contact"`
	EnrollmentID int64  `query:"enrollment_id" minimum:"1" doc:"Only messages of this enrollment"`
	StepID       int64  `query:"step_id" minimum:"1" doc:"Only messages of this sequence step"`
}

type getMessageInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Message ID" example:"1042"`
}

// MessageOutput is a single-message response.
type MessageOutput struct {
	Body MessageDetail
}

type getEventInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Event ID" example:"5310"`
}

// EventOutput is a single-event response.
type EventOutput struct {
	Body Event
}

type getClickInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Click ID" example:"877"`
}

// ClickOutput is a single-click response.
type ClickOutput struct {
	Body Click
}

type listEventsInput struct {
	api.PageParams
	Type      string    `query:"type" enum:"delivery,bounce,complaint,reject,unsubscribe,send_error" doc:"Only events of this type"`
	MessageID int64     `query:"message_id" minimum:"1" doc:"Only events of this message"`
	Since     time.Time `query:"since" doc:"Only events at or after this time (RFC 3339)"`
}

type listClicksInput struct {
	api.PageParams
	ContactID   int64 `query:"contact_id" minimum:"1" doc:"Only clicks by this contact"`
	BroadcastID int64 `query:"broadcast_id" minimum:"1" doc:"Only clicks in messages of this broadcast"`
	LinkID      int64 `query:"link_id" minimum:"1" doc:"Only clicks on this link"`
}

// status returns the Status for st, with the rate the sender uses now.
func (s *Service) status(ctx context.Context, st State) (*StatusOutput, error) {
	out := &StatusOutput{Body: Status{State: st}}
	if a := s.currentAccount(); a != nil {
		out.Body.SESAccount = a
	} else if a, err := s.StoredAccount(ctx, nil); err == nil {
		out.Body.SESAccount = &a
	}
	set, err := s.settings.Get(ctx)
	if err != nil {
		return nil, err
	}
	out.Body.RatePerSec = effectiveRate(set.Sending, out.Body.SESAccount)
	return out, nil
}

// RegisterAPI registers the sending state operations (tag Sending) and the
// read operations for messages, events and clicks.
func (s *Service) RegisterAPI(a *api.API) {
	api.Register(a, huma.Operation{
		OperationID: "get-sending-state",
		Method:      http.MethodGet,
		Path:        "/sending/state",
		Summary:     "Get the sending state",
		Description: "Returns whether all sending is paused (and why, since when and by whom: `manual` or `ses`), the current global send rate and the last SES account snapshot (quota, sending enabled, production access). Sending pauses automatically when SES reports sending paused or disabled, and resumes automatically when SES enables it again.",
		Tags:        []string{"Sending"},
	}, func(ctx context.Context, _ *stateInput) (*StatusOutput, error) {
		st, err := s.GetState(ctx, nil)
		if err != nil {
			return nil, err
		}
		return s.status(ctx, st)
	})

	api.Register(a, huma.Operation{
		OperationID: "pause-sending",
		Method:      http.MethodPost,
		Path:        "/sending/pause",
		Summary:     "Pause all sending",
		Description: "Pauses all sending: no message of any kind (broadcasts, sequence steps, opt-in confirmations, tests, alerts) is sent until resumed. Queued messages are kept. A manual pause is lifted only by POST /sending/resume. Pausing when already paused changes nothing. With `?dry_run=true` nothing is changed and the resulting state is returned.",
		Tags:        []string{"Sending"},
	}, func(ctx context.Context, in *pauseInput) (*StatusOutput, error) {
		reason := ""
		if in.Body != nil {
			reason = in.Body.Reason
		}
		st, err := s.Pause(ctx, reason, in.DryRun)
		if err != nil {
			return nil, err
		}
		return s.status(ctx, st)
	})

	api.Register(a, huma.Operation{
		OperationID: "resume-sending",
		Method:      http.MethodPost,
		Path:        "/sending/resume",
		Summary:     "Resume sending",
		Description: "Lifts a pause, manual or set because of SES. Queued messages are then sent at the normal rate. If SES still reports sending disabled, the next SES account check (every 5 minutes) pauses again. With `?dry_run=true` nothing is changed and the resulting state is returned.",
		Tags:        []string{"Sending"},
	}, func(ctx context.Context, in *resumeInput) (*StatusOutput, error) {
		st, err := s.Resume(ctx, in.DryRun)
		if err != nil {
			return nil, err
		}
		return s.status(ctx, st)
	})

	api.Register(a, huma.Operation{
		OperationID: "list-messages",
		Method:      http.MethodGet,
		Path:        "/messages",
		Summary:     "List messages",
		Description: "Returns messages (one row per email, created before it is sent), newest first, filtered by kind, status, broadcast, contact, enrollment or sequence step. `error` says why a message was skipped (e.g. the contact unsubscribed before it went out) or failed.",
		Tags:        []string{"Messages"},
	}, func(ctx context.Context, in *listMessagesInput) (*api.ListOutput[Message], error) {
		before, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		ms, err := s.ListMessages(ctx, MessageFilter{
			Kind: in.Kind, Status: in.Status, BroadcastID: in.BroadcastID, ContactID: in.ContactID,
			EnrollmentID: in.EnrollmentID, StepID: in.StepID, BeforeID: before, Limit: limit + 1,
		})
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Message]{Body: api.NewList(ms, limit, func(m Message) int64 { return m.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-message",
		Method:      http.MethodGet,
		Path:        "/messages/{id}",
		Summary:     "Get a message",
		Description: "Returns one message with its events (delivery, bounce, complaint, unsubscribe, send errors) and its clicks.",
		Tags:        []string{"Messages"},
	}, func(ctx context.Context, in *getMessageInput) (*MessageOutput, error) {
		m, err := s.GetMessage(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &MessageOutput{Body: m}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "list-events",
		Method:      http.MethodGet,
		Path:        "/events",
		Summary:     "List events",
		Description: "Returns message events, newest first: SES deliveries, bounces, complaints and rejects, unsubscribes, and send errors. Filter by type, message and time. Raw event details are emptied after 90 days.",
		Tags:        []string{"Events"},
	}, func(ctx context.Context, in *listEventsInput) (*api.ListOutput[Event], error) {
		before, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		es, err := s.ListEvents(ctx, EventFilter{Type: in.Type, MessageID: in.MessageID, Since: in.Since, BeforeID: before, Limit: limit + 1})
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Event]{Body: api.NewList(es, limit, func(e Event) int64 { return e.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-event",
		Method:      http.MethodGet,
		Path:        "/events/{id}",
		Summary:     "Get an event",
		Description: "Returns one message event (delivery, bounce, complaint, reject, unsubscribe or send error) with its raw detail (without the original message headers, which carry the recipient's unsubscribe link), which is emptied after 90 days.",
		Tags:        []string{"Events"},
	}, func(ctx context.Context, in *getEventInput) (*EventOutput, error) {
		e, err := s.GetEvent(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &EventOutput{Body: e}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-click",
		Method:      http.MethodGet,
		Path:        "/clicks/{id}",
		Summary:     "Get a click",
		Description: "Returns one tracked link click with the link URL, its message, contact and broadcast.",
		Tags:        []string{"Clicks"},
	}, func(ctx context.Context, in *getClickInput) (*ClickOutput, error) {
		c, err := s.GetClick(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &ClickOutput{Body: c}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "list-clicks",
		Method:      http.MethodGet,
		Path:        "/clicks",
		Summary:     "List clicks",
		Description: "Returns tracked link clicks (per contact, K-1) with the link URL, newest first, filtered by contact, broadcast or link.",
		Tags:        []string{"Clicks"},
	}, func(ctx context.Context, in *listClicksInput) (*api.ListOutput[Click], error) {
		before, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		cs, err := s.ListClicks(ctx, ClickFilter{ContactID: in.ContactID, BroadcastID: in.BroadcastID, LinkID: in.LinkID, BeforeID: before, Limit: limit + 1})
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Click]{Body: api.NewList(cs, limit, func(c Click) int64 { return c.ID })}, nil
	})
}
