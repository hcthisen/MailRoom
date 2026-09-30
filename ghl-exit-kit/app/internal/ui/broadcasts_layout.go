package ui

// The base layout editor (E-1, E-4): logo, brand colour and optional
// custom HTML, with a live preview (preview-layout) of sample content in
// the unsaved layout.

import (
	"database/sql"
	"net/http"
	"strconv"

	"mailroom/internal/content"
)

// layoutPath is the layout editor.
const layoutPath = Prefix + "/layout"

func (u *UI) registerLayout() {
	u.handle(Action{Method: http.MethodGet, Path: layoutPath, OperationID: "get-layout", Uses: []string{"get-settings"},
		Handler: u.layoutPage})
	u.handle(Action{Method: http.MethodPost, Path: layoutPath, OperationID: "update-layout", Uses: []string{"get-settings"}, Handler: u.layoutSave})
	u.handle(Action{Method: http.MethodPost, Path: layoutPath + "/preview", OperationID: "preview-layout", Handler: u.layoutPreview})
}

type layoutData struct {
	Tabs   []SubnavItem
	Form   *Form
	Footer string // settings.company_footer, shown read-only
}

// layoutInput reads the form.
func layoutInput(f *Form) content.LayoutInput {
	return content.LayoutInput{LogoURL: f.Get("logo_url"), BrandColor: f.Get("brand_color"), CustomHTML: f.Raw("custom_html")}
}

func (u *UI) renderLayout(w http.ResponseWriter, r *http.Request, status int, f *Form) error {
	st, err := u.d.Settings.Get(r.Context())
	if err != nil {
		return err
	}
	return u.PageStatus(w, r, status, "broadcasts/layout", View{
		Title: "Base layout", Subtitle: "Every broadcast and sequence email is wrapped in this layout, with the footer and the unsubscribe link.",
		Nav:  "content",
		Data: layoutData{Tabs: contentTabs("layout"), Form: f, Footer: st.CompanyFooter},
	})
}

// layoutPage shows the stored layout.
func (u *UI) layoutPage(w http.ResponseWriter, r *http.Request) error {
	l, err := u.d.Content.GetLayout(r.Context(), nil)
	if err != nil {
		return err
	}
	f := EmptyForm().
		Set("logo_url", l.LogoURL).
		Set("brand_color", l.BrandColor).
		Set("custom_html", l.CustomHTML).
		Set("version", strconv.FormatInt(l.Version, 10))
	return u.renderLayout(w, r, http.StatusOK, f)
}

// layoutSave replaces the layout (update-layout) with the version the
// page was loaded at as If-Match.
func (u *UI) layoutSave(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	ctx := r.Context()
	err := u.tx(ctx, func(tx *sql.Tx) error {
		_, err := u.d.Content.PutLayout(ctx, tx, layoutInput(f), formVersion(f))
		return err
	})
	if err == nil {
		u.Flash(r, FlashSuccess, "Layout saved. Send a test of a broadcast to see it in your inbox.")
		u.Redirect(w, r, layoutPath)
		return nil
	}
	st := f.Fail(err)
	if st == 0 {
		return err
	}
	if st == http.StatusPreconditionFailed {
		if l, gerr := u.d.Content.GetLayout(ctx, nil); gerr == nil {
			f.Set("version", strconv.FormatInt(l.Version, 10))
		}
	}
	return u.renderLayout(w, r, st, f)
}

// layoutPreview renders the sample content in the unsaved layout; a
// validation error (e.g. custom HTML without the unsubscribe link) is
// shown in the preview panel.
func (u *UI) layoutPreview(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	in := layoutInput(f)
	out, err := u.d.Content.RenderDraft(r.Context(), content.LayoutSampleDraft, nil, &in)
	var d previewData
	if err != nil {
		if f.Fail(err) == 0 {
			return err
		}
		d.Error = f.Message
		for field, msg := range f.Errors {
			d.Error = field + ": " + msg
		}
	} else {
		d = previewData{Subject: out.Subject, HTML: out.HTML, Text: out.Text}
	}
	return u.Partial(w, r, "broadcasts/layout", "email-preview", View{Data: d})
}
