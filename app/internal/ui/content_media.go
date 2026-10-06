package ui

// Uploaded images (E-6): list with their public URLs, upload, delete.

import (
	"errors"
	"fmt"
	"net/http"

	"mailroom/internal/content"
)

// mediaUploadLimit is the UI upload's request limit. It is above
// content.MaxMediaBytes on purpose: a slightly too large image then gets
// the service's "larger than 5 MiB" message next to the field instead of
// a failed request.
const mediaUploadLimit = 4 * content.MaxMediaBytes

func (u *UI) registerMedia() {
	u.handle(Action{Method: http.MethodGet, Path: mediaPath, OperationID: "list-media", Handler: u.mediaList})
	u.handle(Action{Method: http.MethodPost, Path: mediaPath, OperationID: "upload-media",
		MaxBodyBytes: mediaUploadLimit, Handler: u.mediaUpload})
	u.handle(Action{Method: http.MethodPost, Path: mediaPath + "/{id}/delete", OperationID: "delete-media",
		Uses: []string{"get-media"}, Handler: u.mediaDelete})
}

type mediaListData struct {
	Tabs  []SubnavItem
	Form  *Form
	Items []content.Media
	Pager Pager
}

func (u *UI) renderMedia(w http.ResponseWriter, r *http.Request, status int, f *Form) error {
	p, err := ParseList(r)
	if err != nil {
		return err
	}
	items, err := u.d.Content.ListMedia(r.Context(), p.After, p.Fetch())
	if err != nil {
		return err
	}
	items, pager := Paginate(r, p, items, func(m content.Media) int64 { return m.ID })
	return u.PageStatus(w, r, status, "content/media", View{
		Title: "Images", Nav: "content",
		Subtitle: "Images for your emails and the layout's logo. Each has a public address to use in an email or as the logo URL.",
		Data:     mediaListData{Tabs: contentTabs("media"), Form: f, Items: items, Pager: pager},
	})
}

// mediaList lists the uploaded images (list-media) with the upload form.
func (u *UI) mediaList(w http.ResponseWriter, r *http.Request) error {
	return u.renderMedia(w, r, http.StatusOK, EmptyForm())
}

// mediaUpload stores an uploaded image (upload-media). The CSRF check
// already parsed the multipart form.
func (u *UI) mediaUpload(w http.ResponseWriter, r *http.Request) error {
	f := EmptyForm()
	file, hdr, err := r.FormFile("file")
	if err != nil {
		if errors.Is(err, http.ErrMissingFile) || errors.Is(err, http.ErrNotMultipart) {
			f.AddError("file", "choose an image file")
			f.Message = "Please correct the highlighted field."
			return u.renderMedia(w, r, http.StatusUnprocessableEntity, f)
		}
		return err
	}
	defer file.Close()
	m, err := u.d.Content.UploadMedia(r.Context(), hdr.Filename, file)
	if err != nil {
		st := f.Fail(err)
		if st == 0 {
			return err
		}
		return u.renderMedia(w, r, st, f)
	}
	u.Flash(r, FlashSuccess, fmt.Sprintf("%s uploaded. Its address is %s", m.Filename, m.URL))
	u.Redirect(w, r, mediaPath)
	return nil
}

// mediaDelete deletes an image (delete-media) after a confirmation. The
// service refuses while an email or the layout refers to it.
func (u *UI) mediaDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	m, err := u.d.Content.GetMedia(ctx, id)
	if err != nil {
		return err
	}
	if !Confirmed(r) {
		return u.RenderConfirm(w, r, Confirm{
			Title: fmt.Sprintf("Delete %s?", m.Filename),
			Message: "The image and its address are deleted for good. Emails that were already sent with it then show a broken image. " +
				"Mailroom refuses while a stored email or the layout still uses the address.",
			Details:      []string{"Address: " + m.URL, "Size: " + formatBytes(m.SizeBytes), "Uploaded: " + u.fmtTime(m.CreatedAt)},
			Action:       fmt.Sprintf("%s/%d/delete", mediaPath, id),
			ConfirmLabel: "Delete image",
			CancelURL:    mediaPath,
			Danger:       true,
			Nav:          "content",
			Crumbs:       []Crumb{{"Images", mediaPath}},
		})
	}
	if err := u.d.Content.DeleteMedia(ctx, id); err != nil {
		f := EmptyForm()
		if f.Fail(err) == 0 {
			return err
		}
		u.Flash(r, FlashError, fmt.Sprintf("%s was not deleted: %s. Change that email or the layout first.", m.Filename, f.Message))
		u.Redirect(w, r, mediaPath)
		return nil
	}
	u.Flash(r, FlashSuccess, fmt.Sprintf("%s deleted.", m.Filename))
	u.Redirect(w, r, mediaPath)
	return nil
}
