package contacts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
	"mailroom/internal/core"
)

// RegisterAPI registers the Contacts, Tags and Suppressions operations.
func (s *Service) RegisterAPI(a *api.API) {
	s.registerContacts(a)
	s.registerContactDetails(a)
	s.registerTags(a)
	s.registerSuppressions(a)
}

// apiErr turns a suppressed-address error into the documented 409.
func apiErr(err error) error {
	if errors.Is(err, core.ErrSuppressed) {
		return api.NewError(http.StatusConflict, api.CodeConflict, "address is suppressed", "email")
	}
	return err
}

// ContactOutput is a single contact with its ETag.
type ContactOutput struct {
	ETag string `header:"ETag" doc:"Version of the contact; send it back in If-Match"`
	Body Contact
}

func contactOutput(c Contact) *ContactOutput {
	return &ContactOutput{ETag: api.ETag(c.Version), Body: c}
}

type contactIDInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Contact ID" example:"42"`
}

type noContent struct{}

type listContactsInput struct {
	api.PageParams
	Q             string    `query:"q" maxLength:"254" doc:"Only contacts whose email, first name, last name or full name contains this (case-insensitive)" example:"smith"`
	Email         string    `query:"email" maxLength:"254" doc:"Only the contact with exactly this address (compared normalised: trimmed and lowercased); look up a contact by email" example:"alice@example.com"`
	Status        string    `query:"status" enum:"pending,active,unsubscribed,bounced,complained,suppressed_manual" doc:"Only contacts with this status"`
	Tag           string    `query:"tag" maxLength:"64" doc:"Only contacts with this tag (case-insensitive)" example:"customer"`
	CreatedAfter  time.Time `query:"created_after" doc:"Only contacts created at or after this time (RFC 3339)"`
	CreatedBefore time.Time `query:"created_before" doc:"Only contacts created before this time (RFC 3339)"`
	Attr          []string  `query:"attr,explode" maxItems:"10" doc:"Attribute filter \"key:value\" (exact match, e.g. attr=company:Acme); repeat for several" example:"company:Acme"`
	SegmentID     int64     `query:"segment_id" minimum:"1" doc:"Only current members of this segment"`
}

type createContactInput struct {
	Body struct {
		Email      string             `json:"email" maxLength:"254" doc:"Email address; stored trimmed and lowercased" example:"alice@example.com"`
		FirstName  string             `json:"first_name,omitempty" maxLength:"200" example:"Alice"`
		LastName   string             `json:"last_name,omitempty" maxLength:"200" example:"Smith"`
		Attributes map[string]any     `json:"attributes,omitempty" doc:"Custom attributes; keys are 1-64 letters, digits or underscores" example:"{\"company\":\"Acme\"}"`
		Tags       []string           `json:"tags,omitempty" maxItems:"100" doc:"Tags to add; created as needed" example:"[\"newsletter\"]"`
		Status     core.ContactStatus `json:"status,omitempty" enum:"pending,active,unsubscribed,bounced,complained,suppressed_manual" doc:"Initial status (default active), e.g. unsubscribed for a migrated contact"`
		Consent    *Consent           `json:"consent,omitempty" doc:"Consent to record in the consent log"`
	}
}

type upsertContactInput struct {
	Body struct {
		Email      string         `json:"email" maxLength:"254" doc:"Email address to match (case-insensitive)" example:"alice@example.com"`
		FirstName  *string        `json:"first_name,omitempty" nullable:"false" maxLength:"200" doc:"Omit to keep the current value" example:"Alice"`
		LastName   *string        `json:"last_name,omitempty" nullable:"false" maxLength:"200" doc:"Omit to keep the current value" example:"Smith"`
		Attributes map[string]any `json:"attributes,omitempty" doc:"Merged into the existing attributes; null removes a key" example:"{\"plan\":\"pro\"}"`
		Tags       []string       `json:"tags,omitempty" maxItems:"100" doc:"Tags to add (existing tags are kept)" example:"[\"customer\"]"`
		Consent    *Consent       `json:"consent,omitempty" doc:"Consent to record in the consent log"`
	}
}

type upsertOutput struct {
	ETag string `header:"ETag" doc:"Version of the contact"`
	Body struct {
		Contact Contact `json:"contact"`
		Created bool    `json:"created" doc:"true when a new contact was created"`
	}
}

type updateContactInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Contact ID" example:"42"`
	api.IfMatchParam
	Body struct {
		FirstName  *string        `json:"first_name,omitempty" nullable:"false" maxLength:"200" example:"Alice"`
		LastName   *string        `json:"last_name,omitempty" nullable:"false" maxLength:"200" example:"Smith"`
		Attributes map[string]any `json:"attributes,omitempty" doc:"Merged into the existing attributes; a null value deletes that key" example:"{\"company\":\"Acme\",\"old_field\":null}"`
	}
}

type setStatusInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Contact ID" example:"42"`
	api.IfMatchParam
	Body struct {
		Status core.ContactStatus `json:"status" enum:"active,unsubscribed,bounced,complained,suppressed_manual" doc:"The new status" example:"unsubscribed"`
		Note   string             `json:"note,omitempty" maxLength:"500" doc:"Why; required to resubscribe, reactivate a bounced contact or confirm a pending one" example:"Customer asked by email"`
	}
}

type setStatusOutput struct {
	ETag string `header:"ETag" doc:"Version of the contact"`
	Body struct {
		Contact Contact `json:"contact"`
		Changed bool    `json:"changed" doc:"false when the contact already had this status (or, for unsubscribed on a bounced, complained or suppressed_manual contact, already had an unsubscribe request recorded)"`
	}
}

type addTagsInput struct {
	ID   int64 `path:"id" minimum:"1" doc:"Contact ID" example:"42"`
	Body struct {
		Tags []string `json:"tags" minItems:"1" maxItems:"100" doc:"Tags to add; created as needed" example:"[\"customer\"]"`
	}
}

type removeTagInput struct {
	ID  int64  `path:"id" minimum:"1" doc:"Contact ID" example:"42"`
	Tag string `path:"tag" maxLength:"64" doc:"Tag name (case-insensitive)" example:"customer"`
}

func (s *Service) registerContacts(a *api.API) {
	tags := []string{"Contacts"}

	api.Register(a, huma.Operation{
		OperationID: "list-contacts", Method: http.MethodGet, Path: "/contacts", Tags: tags,
		Summary:     "List and search contacts",
		Description: "Returns contacts newest first, with their tags. Filters combine with AND: `q` (email or name contains), `email` (exact address: a lookup by email), `status`, `tag`, `created_after`/`created_before`, `attr=key:value` (repeatable, exact match) and `segment_id` (current segment members). Paginate with `limit` and `cursor`.",
	}, func(ctx context.Context, in *listContactsInput) (*api.ListOutput[Contact], error) {
		before, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		f := ListFilter{
			Query: in.Q, Email: in.Email, Status: core.ContactStatus(in.Status), Tag: in.Tag, Attributes: in.Attr,
			SegmentID: in.SegmentID, BeforeID: before, Limit: limit + 1,
		}
		if !in.CreatedAfter.IsZero() {
			f.CreatedAfter = &in.CreatedAfter
		}
		if !in.CreatedBefore.IsZero() {
			f.CreatedBefore = &in.CreatedBefore
		}
		cs, err := s.List(ctx, f)
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Contact]{Body: api.NewList(cs, limit, func(c Contact) int64 { return c.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-contact", Method: http.MethodGet, Path: "/contacts/{id}", Tags: tags,
		Summary:     "Get a contact",
		Description: "Returns one contact with its tags. The ETag header carries the version for If-Match.",
	}, func(ctx context.Context, in *contactIDInput) (*ContactOutput, error) {
		c, err := s.Get(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return contactOutput(c), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "create-contact", Method: http.MethodPost, Path: "/contacts", Tags: tags, DefaultStatus: http.StatusCreated,
		Summary:     "Create a contact",
		Description: "Creates a contact with optional names, attributes and tags (tags are created as needed and can start sequences). `status` defaults to active. A consent event is written to the consent log (type `api` for API tokens, `admin` for UI users, source `api:<token name>[:<consent.source>]`) when `consent` is given or the status is active or pending. An existing email is 409 conflict; a suppressed or erased address is 409 with message \"address is suppressed\".",
	}, func(ctx context.Context, in *createContactInput) (*ContactOutput, error) {
		b := in.Body
		c, err := s.Create(ctx, CreateInput{
			Email: b.Email, FirstName: b.FirstName, LastName: b.LastName, Attributes: b.Attributes,
			Tags: b.Tags, Status: b.Status, Consent: b.Consent,
		})
		if err != nil {
			return nil, apiErr(err)
		}
		return contactOutput(c), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "upsert-contact", Method: http.MethodPost, Path: "/contacts/upsert", Tags: tags,
		Summary:     "Create or update a contact by email",
		Description: "Creates the contact (status active, with a consent event) when the email is new. Otherwise updates the names you send, merges `attributes` (null removes a key) and adds `tags`; a given `consent` is also recorded. The status of an existing contact never changes: use set-contact-status for that. A suppressed or erased address is 409 with message \"address is suppressed\".",
	}, func(ctx context.Context, in *upsertContactInput) (*upsertOutput, error) {
		b := in.Body
		c, created, err := s.Upsert(ctx, UpsertInput{
			Email: b.Email, FirstName: b.FirstName, LastName: b.LastName, Attributes: b.Attributes,
			Tags: b.Tags, Consent: b.Consent,
		})
		if err != nil {
			return nil, apiErr(err)
		}
		out := &upsertOutput{ETag: api.ETag(c.Version)}
		out.Body.Contact, out.Body.Created = c, created
		return out, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "update-contact", Method: http.MethodPatch, Path: "/contacts/{id}", Tags: tags,
		Summary:     "Update a contact",
		Description: "Changes the first name, last name and attributes you send. `attributes` is merged into the existing ones; a null value deletes that key. The email and status can't be changed here. Send the ETag in If-Match to get 412 instead of overwriting a newer change.",
	}, func(ctx context.Context, in *updateContactInput) (*ContactOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		c, err := s.Update(ctx, in.ID, UpdateInput{
			FirstName: in.Body.FirstName, LastName: in.Body.LastName, Attributes: in.Body.Attributes,
		}, v)
		if err != nil {
			return nil, err
		}
		return contactOutput(c), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "set-contact-status", Method: http.MethodPost, Path: "/contacts/{id}/status", Tags: tags,
		Summary: "Change a contact's status",
		Description: "Changes the status, recorded with source `api` (tokens) or `admin` (UI) and the note. The suppression rules apply (403 when refused): " +
			"pending → active, unsubscribed → active and bounced → active need a `note`; complained contacts can never be reactivated; " +
			"nothing moves back to pending; between non-active statuses only bounced/complained can be recorded (and complained over bounced). " +
			"To block an address whatever its status, use add-suppression. Setting the current status is a no-op (`changed: false`). " +
			"The suppressions table follows: suppressed_manual adds the address's hash (reason manual), and suppressed_manual → active removes it. " +
			"Reactivating another contact whose address is suppressed is 409 conflict: remove the suppression first. " +
			"A bounced or suppressed_manual contact that asked to unsubscribe meanwhile becomes unsubscribed instead of active (check `contact.status`). " +
			"Setting `unsubscribed` on a bounced, complained or suppressed_manual contact (a request received by phone or email) keeps its status but records the request: " +
			"the answer is 200 with `changed: true` and `contact.unsubscribe_requested_at` set, and lifting the bounce or suppression later lands on unsubscribed. " +
			"A move to active from pending, unsubscribed or suppressed_manual is appended to the consent log (type api or admin, the note as its text).",
	}, func(ctx context.Context, in *setStatusInput) (*setStatusOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		c, changed, err := s.SetStatus(ctx, in.ID, in.Body.Status, in.Body.Note, v)
		if err != nil {
			return nil, err
		}
		out := &setStatusOutput{ETag: api.ETag(c.Version)}
		out.Body.Contact, out.Body.Changed = c, changed
		return out, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "add-contact-tags", Method: http.MethodPost, Path: "/contacts/{id}/tags", Tags: tags,
		Summary:     "Add tags to a contact",
		Description: "Adds tags (created as needed). A newly added tag can start a sequence (tag trigger) or end one (exit tag), e.g. a payment system adding `customer`. Tags the contact already has are ignored.",
	}, func(ctx context.Context, in *addTagsInput) (*ContactOutput, error) {
		c, err := s.AddTags(ctx, in.ID, in.Body.Tags)
		if err != nil {
			return nil, err
		}
		return contactOutput(c), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "remove-contact-tag", Method: http.MethodDelete, Path: "/contacts/{id}/tags/{tag}", Tags: tags,
		Summary:     "Remove a tag from a contact",
		Description: "Removes one tag (case-insensitive) and returns the contact. Removing a tag the contact doesn't have is not an error.",
	}, func(ctx context.Context, in *removeTagInput) (*ContactOutput, error) {
		c, err := s.RemoveTag(ctx, in.ID, in.Tag)
		if err != nil {
			return nil, err
		}
		return contactOutput(c), nil
	})
}

type timelineInput struct {
	ID     int64  `path:"id" minimum:"1" doc:"Contact ID" example:"42"`
	Limit  int    `query:"limit" minimum:"1" maximum:"500" default:"100" doc:"Maximum number of items (1-500)"`
	Cursor string `query:"cursor" maxLength:"256" doc:"The next_cursor of the previous page"`
}

type timelineOutput struct {
	Body TimelinePage
}

type enrollmentsInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Contact ID" example:"42"`
	api.PageParams
}

type exportOutput struct {
	ContentDisposition string `header:"Content-Disposition"`
	Body               Export
}

type eraseInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Contact ID" example:"42"`
	api.IfMatchParam
	api.DryRunParam
}

type eraseOutput struct {
	Body EraseResult
}

func (s *Service) registerContactDetails(a *api.API) {
	tags := []string{"Contacts"}

	api.Register(a, huma.Operation{
		OperationID: "get-contact-timeline", Method: http.MethodGet, Path: "/contacts/{id}/timeline", Tags: tags,
		Summary:     "Get a contact's timeline",
		Description: "Returns the contact's history newest first: consent events, status changes (unsubscribes, bounces...), tags added, emails (kind, subject, status, times), delivery/bounce/complaint events and clicks. Each item has `at`, `type`, `summary` and `detail`. Paginate with `cursor`.",
	}, func(ctx context.Context, in *timelineInput) (*timelineOutput, error) {
		p, err := s.Timeline(ctx, in.ID, in.Cursor, in.Limit)
		if err != nil {
			return nil, err
		}
		return &timelineOutput{Body: p}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "list-contact-enrollments", Method: http.MethodGet, Path: "/contacts/{id}/enrollments", Tags: tags,
		Summary:     "List a contact's sequence enrollments",
		Description: "Returns the contact's enrollments (active, completed and exited), newest first, with the sequence name and the next step.",
	}, func(ctx context.Context, in *enrollmentsInput) (*api.ListOutput[Enrollment], error) {
		before, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		es, err := s.Enrollments(ctx, in.ID, before, limit+1)
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Enrollment]{Body: api.NewList(es, limit, func(e Enrollment) int64 { return e.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "export-contact", Method: http.MethodGet, Path: "/contacts/{id}/export", Tags: tags,
		Summary:     "Export a contact (GDPR)",
		Description: "Returns everything held about the contact as one JSON document (GDPR access request, C-5): the contact, tags, consent log, status history, messages with their events (also test sends and alerts to its address, and test sends that used its data), other SES events of its address, clicks, opens, enrollments, confirmation requests (pending_optins, with the submitted values, IP and user agent) and form submissions. It covers everything erase-contact removes or anonymises.",
	}, func(ctx context.Context, in *contactIDInput) (*exportOutput, error) {
		ex, err := s.Export(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &exportOutput{
			ContentDisposition: fmt.Sprintf(`attachment; filename="contact-%d.json"`, in.ID),
			Body:               ex,
		}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "erase-contact", Method: http.MethodDelete, Path: "/contacts/{id}", Tags: tags,
		Summary:     "Erase a contact (GDPR)",
		Description: "Deletes the contact and all personal data (C-6): tags, consent log, status history and enrollments. Its messages and clicks are kept for statistics but anonymised: no address, the template subject instead of the personalised one, no error text or raw event data, the recipient in provider event IDs replaced by the hash; queued messages are skipped. Idempotency-Key responses that contain the address are deleted. The SHA-256 of the address is kept in suppressions, so it can't be re-added by an import or form. This can't be undone: try `?dry_run=true` first, which returns the same counts without changing anything.",
	}, func(ctx context.Context, in *eraseInput) (*eraseOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		res, err := s.Erase(ctx, in.ID, v, in.DryRun)
		if err != nil {
			return nil, err
		}
		return &eraseOutput{Body: res}, nil
	})
}
