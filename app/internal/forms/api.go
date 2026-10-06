package forms

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
)

// FormOutput is a single form with its ETag.
type FormOutput struct {
	ETag string `header:"ETag" doc:"Version of the form; send it back in If-Match"`
	Body Form
}

func formOutput(f Form) *FormOutput { return &FormOutput{ETag: api.ETag(f.Version), Body: f} }

type formIDInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Form ID" example:"1"`
}

// formBody is the writable part of a form in create requests.
type formBody struct {
	Name           string      `json:"name" minLength:"1" maxLength:"200" doc:"Name shown as the heading of the hosted page" example:"Newsletter sign-up"`
	Fields         []FormField `json:"fields,omitempty" maxItems:"20" doc:"Inputs besides email (always present and required): first_name, last_name or attr.<key>"`
	Tags           []string    `json:"tags,omitempty" maxItems:"20" doc:"Tags added on sign-up; created as needed" example:"[\"newsletter\"]"`
	SequenceID     *int64      `json:"sequence_id,omitempty" minimum:"0" doc:"Sequence to start on sign-up; omit or 0 for none" example:"2"`
	DoubleOptin    bool        `json:"double_optin,omitempty" doc:"Require a confirmation click before tags and the sequence apply, also for existing active contacts (default false)" example:"false"`
	ConsentText    string      `json:"consent_text,omitempty" maxLength:"2000" doc:"Consent text shown on the form; omit for the default, which says that sign-up means receiving email and that clicks are recorded per person"`
	RedirectURL    string      `json:"redirect_url,omitempty" maxLength:"2048" doc:"Absolute https URL to send visitors to after submitting; omit for the hosted thank-you page" example:"https://example.com/thanks"`
	AllowedOrigins []string    `json:"allowed_origins,omitempty" maxItems:"20" doc:"Web origins allowed to submit with JavaScript (CORS)" example:"[\"https://example.com\"]"`
}

type createFormInput struct {
	api.DryRunParam
	Body formBody
}

type updateFormInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Form ID" example:"1"`
	api.IfMatchParam
	api.DryRunParam
	Body struct {
		Name           *string      `json:"name,omitempty" nullable:"false" minLength:"1" maxLength:"200" example:"Newsletter sign-up"`
		Fields         *[]FormField `json:"fields,omitempty" nullable:"false" maxItems:"20" doc:"Replaces all fields"`
		Tags           *[]string    `json:"tags,omitempty" nullable:"false" maxItems:"20" doc:"Replaces the tags" example:"[\"newsletter\"]"`
		SequenceID     *int64       `json:"sequence_id,omitempty" nullable:"false" minimum:"0" doc:"Sequence to start; 0 removes it" example:"2"`
		DoubleOptin    *bool        `json:"double_optin,omitempty" nullable:"false" example:"true"`
		ConsentText    *string      `json:"consent_text,omitempty" nullable:"false" maxLength:"2000" doc:"Empty restores the default text"`
		RedirectURL    *string      `json:"redirect_url,omitempty" nullable:"false" maxLength:"2048" doc:"Empty uses the hosted thank-you page" example:"https://example.com/thanks"`
		AllowedOrigins *[]string    `json:"allowed_origins,omitempty" nullable:"false" maxItems:"20" doc:"Replaces the allowed origins" example:"[\"https://example.com\"]"`
	}
}

type deleteFormInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Form ID" example:"1"`
	api.IfMatchParam
	api.DryRunParam
}

type submissionsInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Form ID" example:"1"`
	api.PageParams
}

type snippetOutput struct {
	Body Snippet
}

type noContent struct{}

// publicGuide describes the public endpoints in the operations' descriptions.
const publicGuide = "\n\nThe public endpoint `POST /f/{public_id}` takes form-encoded or JSON fields `email`, `first_name`, `last_name` and `attr.<key>` (only the form's fields are used). " +
	"A filled-in `website` field (the hidden honeypot) is treated as spam; each client address may submit 10 times per minute. " +
	"JSON or XMLHttpRequest clients get `{\"ok\": true, \"redirect_url\": \"...\", \"message\": \"...\"}`, browsers a 303 to `redirect_url` or the hosted thank-you page. " +
	"Every accepted submission gets the same answer, also for suppressed, bounced or complained addresses (F-7). " +
	"Every address on a double opt-in form (existing active contacts too) and unsubscribed contacts (always) get a confirmation email (at most one per 10 minutes and 3 per day per address); tags and the sequence apply after the click. " +
	"A submission never overwrites a name or attribute an existing contact already has; it only fills in missing ones. " +
	"Links expire 7 days after the first request; contacts a form created that were never confirmed are then deleted, other pending contacts are kept."

// RegisterAPI registers the Forms operations.
func (s *Service) RegisterAPI(a *api.API) {
	tags := []string{"Forms"}

	api.Register(a, huma.Operation{
		OperationID: "list-forms", Method: http.MethodGet, Path: "/forms", Tags: tags,
		Summary:     "List forms",
		Description: "Returns the sign-up forms ordered by ID, with their hosted page URL and endpoint.",
	}, func(ctx context.Context, in *api.PageParams) (*api.ListOutput[Form], error) {
		after, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		list, err := s.List(ctx, after, limit+1)
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Form]{Body: api.NewList(list, limit, func(f Form) int64 { return f.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "create-form", Method: http.MethodPost, Path: "/forms", Tags: tags, DefaultStatus: http.StatusCreated,
		Summary: "Create a form",
		Description: "Creates a sign-up form with a random public ID; the hosted page is `hosted_url` and submissions go to `endpoint`. " +
			"Double opt-in is off unless `double_optin` is true. `consent_text` defaults to a text saying that signing up means receiving email and that link clicks are recorded per person (K-4). " +
			"`redirect_url` must be an absolute https URL. `allowed_origins` lists the web origins (e.g. `https://example.com`) whose scripts may submit (CORS); plain HTML forms work from anywhere. " +
			"Tags are created as needed; an unknown `sequence_id` is 422. With `?dry_run=true` the form is validated and returned but not saved." + publicGuide,
	}, func(ctx context.Context, in *createFormInput) (*FormOutput, error) {
		b := in.Body
		f, err := s.Create(ctx, FormInput{
			Name: b.Name, Fields: b.Fields, Tags: b.Tags, SequenceID: b.SequenceID, DoubleOptin: b.DoubleOptin,
			ConsentText: b.ConsentText, RedirectURL: b.RedirectURL, AllowedOrigins: b.AllowedOrigins,
		}, in.DryRun)
		if err != nil {
			return nil, err
		}
		return formOutput(f), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-form", Method: http.MethodGet, Path: "/forms/{id}", Tags: tags,
		Summary:     "Get a form",
		Description: "Returns one form. The ETag header carries its version for If-Match.",
	}, func(ctx context.Context, in *formIDInput) (*FormOutput, error) {
		f, err := s.Get(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return formOutput(f), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "update-form", Method: http.MethodPatch, Path: "/forms/{id}", Tags: tags,
		Summary: "Update a form",
		Description: "Changes the members you send; lists (`fields`, `tags`, `allowed_origins`) are replaced as a whole. `sequence_id: 0` removes the sequence, an empty `consent_text` restores the default and an empty `redirect_url` uses the hosted thank-you page. " +
			"The public ID never changes, so embedded forms keep working. Sign-ups waiting for confirmation get the tags and sequence the form has when they click the link. " +
			"Send the ETag in If-Match to get 412 instead of overwriting a concurrent change. With `?dry_run=true` the change is validated and returned but not saved.",
	}, func(ctx context.Context, in *updateFormInput) (*FormOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		b := in.Body
		f, err := s.Update(ctx, in.ID, FormPatch{
			Name: b.Name, Fields: b.Fields, Tags: b.Tags, SequenceID: b.SequenceID, DoubleOptin: b.DoubleOptin,
			ConsentText: b.ConsentText, RedirectURL: b.RedirectURL, AllowedOrigins: b.AllowedOrigins,
		}, v, in.DryRun)
		if err != nil {
			return nil, err
		}
		return formOutput(f), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "delete-form", Method: http.MethodDelete, Path: "/forms/{id}", Tags: tags, DefaultStatus: http.StatusNoContent,
		Summary: "Delete a form",
		Description: "Deletes a form and its submission log. Its public URLs stop working at once (404). Contacts and their consent log stay; sign-ups waiting for confirmation can still confirm and then get the tags and sequence the form had when they signed up. " +
			"Send the ETag in If-Match to get 412 if it changed. With `?dry_run=true` the checks run but nothing is deleted.",
	}, func(ctx context.Context, in *deleteFormInput) (*noContent, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		return &noContent{}, s.Delete(ctx, in.ID, v, in.DryRun)
	})

	api.Register(a, huma.Operation{
		OperationID: "get-form-snippet", Method: http.MethodGet, Path: "/forms/{id}/snippet", Tags: tags,
		Summary: "Get a form's embed snippet",
		Description: "Returns copy-paste HTML for the client's website (F-4): a plain form posting to the endpoint, with the form's fields, the hidden honeypot field and the consent text, unstyled so the site's CSS applies. " +
			"Also returns the hosted page URL for linking to directly.",
	}, func(ctx context.Context, in *formIDInput) (*snippetOutput, error) {
		sn, err := s.Snippet(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &snippetOutput{Body: sn}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "list-form-submissions", Method: http.MethodGet, Path: "/forms/{id}/submissions", Tags: tags,
		Summary: "List a form's recent submissions",
		Description: "Returns the form's submissions newest first with their outcome: `created`, `updated`, `optin_pending`, `resubscribe_pending`, " +
			"`rejected` (suppressed, bounced or complained address; the visitor saw the normal answer), `spam` (honeypot) or `rate_limited`, and the contact when there is one. Paginate with `limit` and `cursor`.",
	}, func(ctx context.Context, in *submissionsInput) (*api.ListOutput[Submission], error) {
		before, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		list, err := s.Submissions(ctx, in.ID, before, limit+1)
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Submission]{Body: api.NewList(list, limit, func(sub Submission) int64 { return sub.ID })}, nil
	})
}

// Snippet returns the embeddable HTML of form id (F-4).
func (s *Service) Snippet(ctx context.Context, id int64) (Snippet, error) {
	f, err := s.Get(ctx, id)
	if err != nil {
		return Snippet{}, err
	}
	html, err := renderSnippet(f)
	if err != nil {
		return Snippet{}, err
	}
	return Snippet{HTML: html, HostedURL: f.HostedURL, Endpoint: f.Endpoint}, nil
}
