package inbound

import (
	"context"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
)

type listInput struct {
	api.PageParams
	Box       string `query:"box" enum:"inbox,unread,archived,all" default:"inbox" doc:"inbox: not archived (default); unread: unread in the inbox; archived; all"`
	ContactID int64  `query:"contact_id" minimum:"0" doc:"Only messages from this contact" example:"42"`
	Q         string `query:"q" maxLength:"200" doc:"Search the subject, sender name and address (case-insensitive)" example:"invoice"`
}

type idPath struct {
	ID int64 `path:"id" minimum:"1" doc:"Received message ID" example:"12"`
}

// MessageOutput is a received message with its ETag.
type MessageOutput struct {
	ETag string `header:"ETag" doc:"Version of the message; send it back in If-Match"`
	Body Message
}

type countsOutput struct{ Body Counts }

type updateInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Received message ID" example:"12"`
	api.IfMatchParam
	Body Update
}

type deleteInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Received message ID" example:"12"`
	api.IfMatchParam
}

type attachmentInput struct {
	ID       int64 `path:"id" minimum:"1" doc:"Received message ID" example:"12"`
	Position int   `path:"position" minimum:"1" doc:"The attachment's position (attachment_list[].position)" example:"1"`
}

type replyInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Received message ID" example:"12"`
	api.DryRunParam
	Body ReplyInput
}

type replyOutput struct {
	Status int
	Body   ReplyResult
}

type noContent struct{}

// RegisterAPI registers the Inbox operations.
func (s *Service) RegisterAPI(a *api.API) {
	tags := []string{"Inbox"}

	api.Register(a, huma.Operation{
		OperationID: "list-inbound", Method: http.MethodGet, Path: "/inbound", Tags: tags,
		Summary: "List received email",
		Description: "Returns the email received for the inbound domain, newest first, without bodies (N-3). `box` selects the inbox (not archived, the default), unread, archived or all. " +
			"Filter by `contact_id` or search with `q`. Messages with status pending were received by SES but not fetched yet; failed ones could not be fetched (see get-inbound's fetch_error). " +
			"The cursor pages backwards: pass next_cursor to get older messages.",
	}, func(ctx context.Context, in *listInput) (*api.ListOutput[Summary], error) {
		before, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		list, err := s.List(ctx, ListFilter{Box: in.Box, ContactID: in.ContactID, Query: in.Q, BeforeID: before, Limit: limit + 1})
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Summary]{Body: api.NewList(list, limit, func(m Summary) int64 { return m.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-inbound-counts", Method: http.MethodGet, Path: "/inbound/counts", Tags: tags,
		Summary:     "Count received email",
		Description: "Returns whether receiving is set up and the inbox counts: unread, in the inbox, pending (not fetched yet) and failed.",
	}, func(ctx context.Context, _ *struct{}) (*countsOutput, error) {
		c, err := s.Counts(ctx)
		if err != nil {
			return nil, err
		}
		return &countsOutput{Body: c}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-inbound", Method: http.MethodGet, Path: "/inbound/{id}", Tags: tags,
		Summary: "Get a received email",
		Description: "Returns one received message with its text and HTML bodies, the SES spam, virus, SPF, DKIM and DMARC verdicts, the attachment list and the replies sent from Mailroom. " +
			"The HTML is as received and untrusted: show it only sandboxed. Reading it doesn't mark it read; use update-inbound.",
	}, func(ctx context.Context, in *idPath) (*MessageOutput, error) {
		m, err := s.Get(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &MessageOutput{ETag: api.ETag(m.Version), Body: m}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "update-inbound", Method: http.MethodPatch, Path: "/inbound/{id}", Tags: tags,
		Summary:     "Mark a received email read, unread or archived",
		Description: "Sets `read` and/or `archived`; omitted fields stay. Send the ETag in If-Match to get 412 instead of overwriting a concurrent change.",
	}, func(ctx context.Context, in *updateInput) (*MessageOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		m, err := s.Update(ctx, in.ID, in.Body, v)
		if err != nil {
			return nil, err
		}
		return &MessageOutput{ETag: api.ETag(m.Version), Body: m}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "delete-inbound", Method: http.MethodDelete, Path: "/inbound/{id}", Tags: tags, DefaultStatus: http.StatusNoContent,
		Summary:     "Delete a received email",
		Description: "Deletes a received message and its attachments for good. Replies sent to it stay in the messages. Send the ETag in If-Match to get 412 if it changed.",
	}, func(ctx context.Context, in *deleteInput) (*noContent, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		return &noContent{}, s.Delete(ctx, in.ID, v)
	})

	api.Register(a, huma.Operation{
		OperationID: "get-inbound-attachment", Method: http.MethodGet, Path: "/inbound/{id}/attachments/{position}", Tags: tags,
		Summary: "Download an attachment",
		Description: "Returns one attachment of a received message as a download (Content-Disposition: attachment) with its declared type. " +
			"Attachments come from anyone who can send email to the domain: treat them as untrusted.",
		Responses: map[string]*huma.Response{
			"200": {
				Description: "The attachment",
				Content:     map[string]*huma.MediaType{"application/octet-stream": {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}}},
			},
		},
	}, func(ctx context.Context, in *attachmentInput) (*huma.StreamResponse, error) {
		att, data, err := s.AttachmentData(ctx, in.ID, in.Position)
		if err != nil {
			return nil, err
		}
		return &huma.StreamResponse{Body: func(hc huma.Context) {
			WriteAttachmentHeaders(hc.SetHeader, att, len(data), false)
			hc.SetStatus(http.StatusOK)
			_, _ = hc.BodyWriter().Write(data)
		}}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "reply-inbound", Method: http.MethodPost, Path: "/inbound/{id}/reply", Tags: tags, DefaultStatus: http.StatusAccepted,
		Metadata: map[string]any{api.MetaAlsoResponds: map[int]string{
			http.StatusOK: "Dry run (?dry_run=true): where the reply would go and its subject; nothing was queued"}},
		Summary: "Reply to a received email",
		Description: "Queues a plain-text reply (message kind reply) from the settings sender to the message's Reply-To address, or else its sender (N-5). It is threaded with In-Reply-To and References and quotes the received text unless `quote` is false. " +
			"It has no layout, unsubscribe link or tracking, and doesn't count in any stats. Refused with 409 when the address is suppressed or its contact bounced or complained, and for a message that isn't fetched yet. " +
			"The message is marked read. Answers 202 with the queued message ID (GET /messages/{id} shows its delivery); `?dry_run=true` queues nothing and answers 200.",
	}, func(ctx context.Context, in *replyInput) (*replyOutput, error) {
		res, err := s.Reply(ctx, in.ID, in.Body, in.DryRun)
		if err != nil {
			return nil, err
		}
		status := http.StatusAccepted
		if in.DryRun {
			status = http.StatusOK
		}
		return &replyOutput{Status: status, Body: res}, nil
	})
}

// WriteAttachmentHeaders sets the download headers of an attachment.
// inline true allows images to be shown in place (the UI's cid: images);
// everything else is a download. The type is the declared one, except
// that anything a browser would run or render as a page is sent as
// application/octet-stream.
func WriteAttachmentHeaders(set func(name, value string), a Attachment, size int, inline bool) {
	ctype := strings.ToLower(a.ContentType)
	if !safeType(ctype) {
		ctype = "application/octet-stream"
	}
	disp := "attachment"
	if inline && strings.HasPrefix(ctype, "image/") {
		disp = "inline"
	}
	set("Content-Type", ctype)
	set("Content-Length", strconv.Itoa(size))
	set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": a.Filename}))
	set("X-Content-Type-Options", "nosniff")
	set("Content-Security-Policy", "default-src 'none'; sandbox")
	set("Cache-Control", "private, no-store")
}

// safeType reports whether a declared attachment type may be served as
// is: images other than SVG, PDF, plain text and common documents.
func safeType(t string) bool {
	switch {
	case t == "image/svg+xml":
		return false
	case strings.HasPrefix(t, "image/"), t == "application/pdf", t == "text/plain", t == "text/csv", t == "text/calendar":
		return true
	case strings.HasPrefix(t, "application/vnd.openxmlformats-officedocument."), t == "application/msword",
		t == "application/vnd.ms-excel", t == "application/zip":
		return true
	}
	return false
}
