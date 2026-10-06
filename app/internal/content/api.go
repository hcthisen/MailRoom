package content

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
	"mailroom/internal/core"
)

// maxJSONBodyBytes bounds request bodies that carry an email body (1 MiB
// of content can grow when JSON-escaped).
const maxJSONBodyBytes = 4 << 20

// readScope marks read-only POST operations (previews).
func readScope() map[string]any { return map[string]any{api.MetaScope: api.ScopeRead} }

type noContent struct{}

// emailPath is the input of operations on /emails/{id}. It is used as a
// whole input, never embedded: Huma ignores params of unexported embedded
// structs.
type emailPath struct {
	ID int64 `path:"id" minimum:"1" doc:"Email ID" example:"3"`
}

// EmailOutput is a single email with its ETag.
type EmailOutput struct {
	ETag string `header:"ETag" doc:"Version of the email; send it back in If-Match"`
	Body Email
}

func emailOutput(e Email) *EmailOutput { return &EmailOutput{ETag: api.ETag(e.Version), Body: e} }

type emailUsageOutput struct {
	Body EmailUsage
}

type listEmailsInput struct {
	api.PageParams
	Query string `query:"q" maxLength:"200" doc:"Only emails whose name or subject contains this text (case-insensitive)" example:"welcome"`
}

// EmailBody is the request body of create-email.
type EmailBody struct {
	Name      string `json:"name,omitempty" maxLength:"200" doc:"Internal name, shown in lists" example:"Welcome 1"`
	Subject   string `json:"subject" maxLength:"300" doc:"Subject line (required); may use merge fields" example:"Welcome, {{first_name | \"friend\"}}!"`
	Preheader string `json:"preheader,omitempty" maxLength:"300" doc:"Preview text shown after the subject in the inbox" example:"Here is what happens next"`
	Format    string `json:"format,omitempty" enum:"markdown,html" doc:"markdown (default) or html (raw HTML, e.g. copied from GHL)" example:"markdown"`
	Body      string `json:"body" doc:"The content, at most 1 MiB. Merge fields: {{first_name}}, {{last_name}}, {{email}}, {{attr.<key>}}, {{unsubscribe_url}}, each with an optional fallback like {{first_name | \"there\"}}" example:"Hi {{first_name | \"there\"}},\n\nThanks for joining."`
}

type createEmailInput struct {
	api.DryRunParam
	Body EmailBody
}

type updateEmailInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Email ID" example:"3"`
	api.IfMatchParam
	api.DryRunParam
	Body struct {
		Name      *string `json:"name,omitempty" nullable:"false" maxLength:"200" example:"October newsletter"`
		Subject   *string `json:"subject,omitempty" nullable:"false" maxLength:"300" example:"Hi {{first_name | \"there\"}}, our October news"`
		Preheader *string `json:"preheader,omitempty" nullable:"false" maxLength:"300" example:"Three things we shipped this month"`
		Format    *string `json:"format,omitempty" nullable:"false" enum:"markdown,html" example:"markdown"`
		Body      *string `json:"body,omitempty" nullable:"false" example:"Hi {{first_name | \"there\"}}, here is what's new this month."`
	}
}

type deleteEmailInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Email ID" example:"3"`
	api.IfMatchParam
}

// PreviewBody is a rendered email.
type PreviewBody struct {
	Subject string `json:"subject" doc:"The rendered subject" example:"Welcome, Alice!"`
	HTML    string `json:"html" doc:"The complete HTML as sent, wrapped in the layout; the unsubscribe link is #preview and links are not tracked"`
	Text    string `json:"text" doc:"The generated plain-text version"`
}

type previewOutput struct {
	Body PreviewBody
}

func preview(r core.Rendered) *previewOutput {
	return &previewOutput{Body: PreviewBody{Subject: r.Subject, HTML: r.HTML, Text: r.Text}}
}

type previewContact struct {
	ContactID int64 `json:"contact_id,omitempty" minimum:"0" doc:"Render with this contact's data; omit to show every merge field's fallback" example:"42"`
}

type previewEmailInput struct {
	ID   int64 `path:"id" minimum:"1" doc:"Email ID" example:"3"`
	Body *previewContact
}

type renderEmailInput struct {
	Body struct {
		EmailBody
		ContactID int64 `json:"contact_id,omitempty" minimum:"0" doc:"Render with this contact's data; omit to show every merge field's fallback" example:"42"`
	}
}

type testEmailInput struct {
	ID   int64 `path:"id" minimum:"1" doc:"Email ID" example:"3"`
	Body struct {
		To        []string `json:"to,omitempty" maxItems:"5" doc:"1-5 addresses; omit to send to the owner's address from the settings" example:"[\"owner@example.org\"]"`
		ContactID int64    `json:"contact_id,omitempty" minimum:"0" doc:"Fill merge fields with this contact's data; omit to use the fallbacks" example:"42"`
	}
}

type testEmailOutput struct {
	Body struct {
		MessageIDs []int64  `json:"message_ids" doc:"The queued test messages, one per address" example:"[901]"`
		To         []string `json:"to" doc:"The normalised addresses, in the same order" example:"[\"owner@example.org\"]"`
	}
}

// previewContactByID loads the contact for a preview; 0 means none.
func (s *Service) previewContactByID(ctx context.Context, id int64) (*core.Contact, error) {
	if id <= 0 {
		return nil, nil
	}
	c, err := s.contacts.Get(ctx, nil, id)
	if errors.Is(err, core.ErrNotFound) {
		return nil, invalid("contact_id", "contact %d does not exist", id)
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// RegisterAPI registers the emails, layout and media operations.
func (s *Service) RegisterAPI(a *api.API) {
	s.registerEmails(a)
	s.registerLayout(a)
	s.registerMedia(a)
}

func (s *Service) registerEmails(a *api.API) {
	tags := []string{"Emails"}
	api.Register(a, huma.Operation{
		OperationID: "list-emails", Method: http.MethodGet, Path: "/emails", Tags: tags,
		Summary:     "List emails",
		Description: "Returns the email library (content of broadcasts and sequence steps) without bodies, ordered by ID. Filter with q (name or subject).",
	}, func(ctx context.Context, in *listEmailsInput) (*api.ListOutput[EmailSummary], error) {
		after, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		es, err := s.ListEmails(ctx, ListFilter{AfterID: after, Limit: limit + 1, Query: in.Query})
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[EmailSummary]{Body: api.NewList(es, limit, func(e EmailSummary) int64 { return e.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "create-email", Method: http.MethodPost, Path: "/emails", Tags: tags, DefaultStatus: http.StatusCreated,
		MaxBodyBytes: maxJSONBodyBytes,
		Summary:      "Create an email",
		Description: "Creates an email in Markdown (default) or raw HTML (E-7: paste HTML copied from GHL; a whole document is fine, its body is wrapped in the layout). " +
			"Merge fields are checked on save: {{first_name}}, {{last_name}}, {{email}}, {{attr.<key>}} and {{unsubscribe_url}}, each with an optional fallback such as {{first_name | \"there\"}}; anything else is a 422. " +
			"With ?dry_run=true nothing is saved and the email that would be created is returned.",
	}, func(ctx context.Context, in *createEmailInput) (*EmailOutput, error) {
		var e Email
		err := api.Tx(ctx, s.db, in.DryRun, func(tx *sql.Tx) error {
			var err error
			e, err = s.CreateEmail(ctx, tx, EmailInput{Name: in.Body.Name, Subject: in.Body.Subject,
				Preheader: in.Body.Preheader, Format: in.Body.Format, Body: in.Body.Body})
			return err
		})
		if err != nil {
			return nil, err
		}
		return emailOutput(e), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-email", Method: http.MethodGet, Path: "/emails/{id}", Tags: tags,
		Summary:     "Get an email",
		Description: "Returns one email with its body. The ETag header carries its version for If-Match.",
	}, func(ctx context.Context, in *emailPath) (*EmailOutput, error) {
		e, err := s.GetEmail(ctx, nil, in.ID)
		if err != nil {
			return nil, err
		}
		return emailOutput(e), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-email-usage", Method: http.MethodGet, Path: "/emails/{id}/usage", Tags: tags,
		Summary:     "Show what uses an email",
		Description: "Lists the sequence steps that send the email, the broadcasts whose content it is, and how many messages with it are still waiting to be sent. delete-email is refused while any of them exist.",
	}, func(ctx context.Context, in *emailPath) (*emailUsageOutput, error) {
		u, err := s.EmailUsage(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &emailUsageOutput{Body: u}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "update-email", Method: http.MethodPatch, Path: "/emails/{id}", Tags: tags,
		MaxBodyBytes: maxJSONBodyBytes,
		Summary:      "Update an email",
		Description: "Changes the fields you send; the rest stay. Changes apply to every future send, including sequence steps using this email. " +
			"Merge fields are checked as on create. The email of a broadcast that is sending, paused, sent or cancelled can't change (409): duplicate the broadcast instead. " +
			"Send If-Match to get 412 instead of overwriting a concurrent change. With ?dry_run=true nothing is saved.",
	}, func(ctx context.Context, in *updateEmailInput) (*EmailOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		var e Email
		err = api.Tx(ctx, s.db, in.DryRun, func(tx *sql.Tx) error {
			var err error
			e, err = s.UpdateEmail(ctx, tx, in.ID, EmailPatch{Name: in.Body.Name, Subject: in.Body.Subject,
				Preheader: in.Body.Preheader, Format: in.Body.Format, Body: in.Body.Body}, v)
			return err
		})
		if err != nil {
			return nil, err
		}
		return emailOutput(e), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "delete-email", Method: http.MethodDelete, Path: "/emails/{id}", Tags: tags, DefaultStatus: http.StatusNoContent,
		Summary:     "Delete an email",
		Description: "Deletes an email. It is refused with 409 while a sequence step or a broadcast (of any status) uses it, or test messages with it are still waiting to be sent.",
	}, func(ctx context.Context, in *deleteEmailInput) (*noContent, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		return &noContent{}, s.db.WithTx(ctx, func(tx *sql.Tx) error { return s.DeleteEmail(ctx, tx, in.ID, v) })
	})

	api.Register(a, huma.Operation{
		OperationID: "preview-email", Method: http.MethodPost, Path: "/emails/{id}/preview", Tags: tags,
		Metadata:    readScope(),
		Summary:     "Preview an email",
		Description: "Renders a stored email as it would be sent: subject, full HTML in the layout and the plain-text version. Pass contact_id to fill merge fields with a real contact's data (B-7). Nothing is saved, links aren't tracked and the unsubscribe link is #preview.",
	}, func(ctx context.Context, in *previewEmailInput) (*previewOutput, error) {
		var contactID int64
		if in.Body != nil {
			contactID = in.Body.ContactID
		}
		c, err := s.previewContactByID(ctx, contactID)
		if err != nil {
			return nil, err
		}
		em, err := s.GetEmail(ctx, nil, in.ID)
		if err != nil {
			return nil, err
		}
		r, err := s.renderDraft(ctx, Draft{Subject: em.Subject, Preheader: em.Preheader, Format: em.Format, Body: em.Body},
			core.RenderInput{Contact: c, Marketing: true}, nil, false)
		if err != nil {
			return nil, err
		}
		return preview(r), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "render-email", Method: http.MethodPost, Path: "/emails/render", Tags: tags,
		Metadata: readScope(), MaxBodyBytes: maxJSONBodyBytes,
		Summary:     "Render unsaved content",
		Description: "Renders subject, preheader and body as they would be sent, without saving anything (E-2 live preview). The content is validated like on save (422 on unknown merge fields). Pass contact_id to use a real contact's data.",
	}, func(ctx context.Context, in *renderEmailInput) (*previewOutput, error) {
		c, err := s.previewContactByID(ctx, in.Body.ContactID)
		if err != nil {
			return nil, err
		}
		b := in.Body.EmailBody
		r, err := s.RenderDraft(ctx, Draft{Subject: b.Subject, Preheader: b.Preheader, Format: b.Format, Body: b.Body}, c, nil)
		if err != nil {
			return nil, err
		}
		return preview(r), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "send-test-email", Method: http.MethodPost, Path: "/emails/{id}/test", Tags: tags, DefaultStatus: http.StatusAccepted,
		Summary: "Send a test",
		Description: "Queues a test send of the email to 1-5 addresses (B-7), or to the owner's address when to is omitted (S-10). With contact_id the merge fields use that contact's data, otherwise their fallbacks. " +
			"Test messages go through the normal sender (skipped for suppressed addresses and for addresses whose contact is unsubscribed, bounced, complained or suppressed), don't count in any stats and carry an inactive unsubscribe link. Returns the queued message IDs.",
	}, func(ctx context.Context, in *testEmailInput) (*testEmailOutput, error) {
		out := &testEmailOutput{}
		err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			out.Body.MessageIDs, out.Body.To, err = s.SendTest(ctx, tx, in.ID, in.Body.To, in.Body.ContactID)
			return err
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	})
}
