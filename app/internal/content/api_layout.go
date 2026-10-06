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

// LayoutOutput is the layout with its ETag.
type LayoutOutput struct {
	ETag string `header:"ETag" doc:"Version of the layout; send it back in If-Match"`
	Body Layout
}

func layoutOutput(l Layout) *LayoutOutput { return &LayoutOutput{ETag: api.ETag(l.Version), Body: l} }

type putLayoutInput struct {
	api.IfMatchParam
	api.DryRunParam
	Body LayoutInput
}

type previewLayoutInput struct {
	Body struct {
		LayoutInput
		EmailID   int64 `json:"email_id,omitempty" minimum:"0" doc:"Show this email in the layout; omit for sample content" example:"3"`
		ContactID int64 `json:"contact_id,omitempty" minimum:"0" doc:"Fill merge fields with this contact's data" example:"42"`
	}
}

// LayoutSampleDraft is the content of layout previews without an email
// (preview-layout; the admin UI's layout editor shows the same).
var LayoutSampleDraft = Draft{
	Subject:   "Layout preview",
	Preheader: "This is the preview text shown in the inbox",
	Format:    FormatMarkdown,
	Body: "# Your headline\n\n" +
		"Hi {{first_name | \"there\"}},\n\n" +
		"This is how your emails look in this layout: paragraphs, **bold** and *italic* text, [links](https://example.com) and lists.\n\n" +
		"- First point\n- Second point\n\n" +
		"Thanks for reading!",
}

func (s *Service) registerLayout(a *api.API) {
	tags := []string{"Layout"}
	api.Register(a, huma.Operation{
		OperationID: "get-layout", Method: http.MethodGet, Path: "/layout", Tags: tags,
		Summary:     "Get the base layout",
		Description: "Returns the base layout every marketing email is wrapped in (E-1): logo, brand colour and optional custom HTML. The footer text comes from settings.company_footer, and the unsubscribe link is always added (E-4). The ETag header carries its version for If-Match.",
	}, func(ctx context.Context, _ *struct{}) (*LayoutOutput, error) {
		l, err := s.GetLayout(ctx, nil)
		if err != nil {
			return nil, err
		}
		return layoutOutput(l), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "update-layout", Method: http.MethodPut, Path: "/layout", Tags: tags,
		MaxBodyBytes: maxJSONBodyBytes,
		Summary:      "Replace the base layout",
		Description: "Replaces the base layout. logo_url must be an absolute http(s) URL (upload the logo with upload-media and use its url), brand_color a #rrggbb colour. " +
			"custom_html replaces the built-in responsive layout: it must contain {{content}} once, a link to {{unsubscribe_url}} (E-4) and {{footer}} outside comments and outside hidden elements (the company name and postal address from the settings, E-1; hidden means the hidden attribute, display:none, visibility:hidden, font-size:0, opacity:0, mso-hide:all or a zero height with overflow:hidden); saving is refused otherwise. It may use {{preheader}}, {{logo_url}} and {{brand_color}}. Send an empty custom_html to use the built-in layout. " +
			"Send If-Match to get 412 instead of overwriting a concurrent change. With ?dry_run=true nothing is saved.",
	}, func(ctx context.Context, in *putLayoutInput) (*LayoutOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		var l Layout
		err = api.Tx(ctx, s.db, in.DryRun, func(tx *sql.Tx) error {
			var err error
			l, err = s.PutLayout(ctx, tx, in.Body, v)
			return err
		})
		if err != nil {
			return nil, err
		}
		return layoutOutput(l), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "preview-layout", Method: http.MethodPost, Path: "/layout/preview", Tags: tags,
		Metadata: readScope(), MaxBodyBytes: maxJSONBodyBytes,
		Summary:     "Preview a layout",
		Description: "Renders a layout without saving it, with sample content or the email given by email_id, and returns subject, HTML and plain text. The layout is validated like on save.",
	}, func(ctx context.Context, in *previewLayoutInput) (*previewOutput, error) {
		c, err := s.previewContactByID(ctx, in.Body.ContactID)
		if err != nil {
			return nil, err
		}
		d := LayoutSampleDraft
		if in.Body.EmailID > 0 {
			em, err := s.GetEmail(ctx, nil, in.Body.EmailID)
			if err != nil {
				if errors.Is(err, core.ErrNotFound) {
					return nil, invalid("email_id", "email %d does not exist", in.Body.EmailID)
				}
				return nil, err
			}
			d = Draft{Subject: em.Subject, Preheader: em.Preheader, Format: em.Format, Body: em.Body}
		}
		layout := in.Body.LayoutInput
		r, err := s.RenderDraft(ctx, d, c, &layout)
		if err != nil {
			return nil, err
		}
		return preview(r), nil
	})
}
