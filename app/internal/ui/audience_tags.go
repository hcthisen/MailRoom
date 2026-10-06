package ui

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"mailroom/internal/contacts"
	"mailroom/internal/core"
)

type audTagsData struct {
	Tabs   template.HTML
	Q      string
	Tags   []contacts.Tag
	Pager  Pager
	Create *Form
}

// audTagsList lists tags with their contact counts (list-tags), with the
// create form.
func (u *UI) audTagsList(w http.ResponseWriter, r *http.Request) error {
	return u.audRenderTags(w, r, http.StatusOK, EmptyForm())
}

func (u *UI) audRenderTags(w http.ResponseWriter, r *http.Request, status int, create *Form) error {
	p, err := ParseList(r)
	if err != nil {
		return err
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	ts, err := u.d.Contacts.ListTags(r.Context(), q, p.After, p.Fetch())
	if err != nil {
		return err
	}
	d := audTagsData{Tabs: audTabs("tags"), Q: q, Create: create}
	d.Tags, d.Pager = Paginate(r, p, ts, func(t contacts.Tag) int64 { return t.ID })
	return u.PageStatus(w, r, status, "audience/tags", View{Title: "Tags", Nav: "contacts",
		Subtitle: "Tags group contacts. Adding a tag can start a sequence.", Data: d})
}

// audTagCreate creates an empty tag (create-tag).
func (u *UI) audTagCreate(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	t, err := u.d.Contacts.CreateTag(r.Context(), f.Get("name"))
	if err == nil {
		u.Flash(r, FlashSuccess, "Tag "+t.Name+" created.")
		u.Redirect(w, r, Prefix+"/tags")
		return nil
	}
	if st := f.Fail(err); st != 0 {
		return u.audRenderTags(w, r, st, f)
	}
	return err
}

func audTagURL(id int64) string { return fmt.Sprintf("%s/tags/%d", Prefix, id) }

type audTagData struct {
	Tag        contacts.Tag
	RenameForm *Form
	MergeForm  *Form
	Tags       []string // merge target suggestions
}

// audTagShow is a tag's page: rename, merge and delete (get-tag).
func (u *UI) audTagShow(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	return u.audRenderTag(w, r, id, http.StatusOK, nil, nil)
}

func (u *UI) audRenderTag(w http.ResponseWriter, r *http.Request, id int64, status int, rename, merge *Form) error {
	t, err := u.d.Contacts.GetTag(r.Context(), id)
	if err != nil {
		return err
	}
	if rename == nil {
		rename = EmptyForm().Set("name", t.Name)
	}
	if merge == nil {
		merge = EmptyForm()
	}
	var names []string
	for _, n := range u.audTagNames(r.Context()) {
		if !strings.EqualFold(n, t.Name) {
			names = append(names, n)
		}
	}
	return u.PageStatus(w, r, status, "audience/tag", View{Title: "Tag " + t.Name, Nav: "contacts",
		Subtitle: plural(t.Contacts, "contact has", "contacts have") + " this tag.",
		Crumbs:   []Crumb{{"Tags", Prefix + "/tags"}},
		Data:     audTagData{Tag: t, RenameForm: rename, MergeForm: merge, Tags: names}})
}

// audTagRename renames a tag (rename-tag); saved segments follow.
func (u *UI) audTagRename(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	f := NewForm(r)
	t, err := u.d.Contacts.RenameTag(r.Context(), id, f.Get("name"))
	if err == nil {
		u.Flash(r, FlashSuccess, "Tag renamed to "+t.Name+".")
		u.Redirect(w, r, audTagURL(id))
		return nil
	}
	if st := f.Fail(err); st != 0 {
		return u.audRenderTag(w, r, id, st, f, nil)
	}
	return err
}

// audFindTag resolves the merge target by name (list-tags).
func (u *UI) audFindTag(r *http.Request, name string) (contacts.Tag, error) {
	if name == "" {
		return contacts.Tag{}, fieldError("into", "enter the tag to merge into")
	}
	ts, err := u.d.Contacts.ListTags(r.Context(), name, 0, 100)
	if err != nil {
		return contacts.Tag{}, err
	}
	for _, t := range ts {
		if strings.EqualFold(t.Name, name) {
			return t, nil
		}
	}
	return contacts.Tag{}, fieldError("into", "no tag has this name")
}

// audIntoField moves the service's into_tag_id errors to the form's
// "into" (name) input.
func audIntoField(err error) error {
	var ve *core.ValidationError
	if errors.As(err, &ve) && ve.Field == "into_tag_id" {
		return &core.ValidationError{Field: "into", Message: ve.Message}
	}
	return err
}

// audTagMerge merges the tag into another (merge-tag) after a
// confirmation. When the service refuses because enrolled contacts would
// gain an exit tag, a second confirmation offers to merge with force.
func (u *UI) audTagMerge(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	f := NewForm(r)
	src, err := u.d.Contacts.GetTag(ctx, id)
	if err != nil {
		return err
	}
	crumbs := []Crumb{{"Tags", Prefix + "/tags"}, {src.Name, audTagURL(id)}}
	into, err := u.audFindTag(r, f.Get("into"))
	if err == nil && into.ID == id {
		err = fieldError("into", "must be a different tag")
	}
	if err != nil {
		if st := f.Fail(err); st != 0 {
			return u.audRenderTag(w, r, id, st, nil, f)
		}
		return err
	}
	force := f.Checked("force")
	if !Confirmed(r) {
		return u.RenderConfirm(w, r, Confirm{
			Title:   fmt.Sprintf("Merge %q into %q?", src.Name, into.Name),
			Message: "The tag " + src.Name + " is deleted after its contacts are moved. This can't be undone.",
			Details: []string{
				plural(src.Contacts, "contact", "contacts") + " with " + src.Name + " get " + into.Name + " (no sequence triggers fire).",
				"Sequences, forms and saved segments that use " + src.Name + " switch to " + into.Name + ".",
			},
			Action: audTagURL(id) + "/merge", Fields: f.Values, ConfirmLabel: "Merge tags",
			CancelURL: audTagURL(id), Danger: true, Nav: "contacts", Crumbs: crumbs,
		})
	}
	res, err := u.d.Contacts.MergeTag(ctx, id, into.ID, force)
	if err != nil {
		if !force && errors.Is(err, core.ErrConflict) && strings.Contains(err.Error(), "with force") {
			fields := f.Values
			fields.Set("force", "1")
			return u.RenderConfirm(w, r, Confirm{
				Title:   "Merge anyway?",
				Message: strings.TrimSuffix(err.Error(), ": "+core.ErrConflict.Error()),
				Details: []string{"With force those contacts leave the sequence before their next step."},
				Action:  audTagURL(id) + "/merge", Fields: fields, ConfirmLabel: "Merge anyway",
				CancelURL: audTagURL(id), Danger: true, Nav: "contacts", Crumbs: crumbs,
			})
		}
		if st := f.Fail(audIntoField(err)); st != 0 {
			return u.audRenderTag(w, r, id, st, nil, f)
		}
		return err
	}
	msg := fmt.Sprintf("Merged %s into %s: %s moved.", src.Name, res.Tag.Name, plural(res.Moved, "contact", "contacts"))
	if res.ExitingEnrollments > 0 {
		msg += " " + plural(res.ExitingEnrollments, "enrollment ends", "enrollments end") + " before the next step."
	}
	u.Flash(r, FlashSuccess, msg)
	u.Redirect(w, r, audTagURL(res.Tag.ID))
	return nil
}

// audTagDelete deletes a tag (delete-tag) after a confirmation that says
// how many contacts lose it.
func (u *UI) audTagDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	t, err := u.d.Contacts.GetTag(ctx, id)
	if err != nil {
		return err
	}
	if !Confirmed(r) {
		return u.RenderConfirm(w, r, Confirm{
			Title:   fmt.Sprintf("Delete tag %q?", t.Name),
			Message: "The tag is removed from every contact. A tag used by a sequence, form or saved segment can't be deleted.",
			Details: []string{plural(t.Contacts, "contact loses", "contacts lose") + " this tag."},
			Action:  audTagURL(id) + "/delete", ConfirmLabel: "Delete tag", CancelURL: audTagURL(id),
			Danger: true, Nav: "contacts", Crumbs: []Crumb{{"Tags", Prefix + "/tags"}, {t.Name, audTagURL(id)}},
		})
	}
	if err := u.d.Contacts.DeleteTag(ctx, id); err != nil {
		f := EmptyForm()
		if f.Fail(err) == 0 {
			return err
		}
		u.Flash(r, FlashError, audFormMessage(f))
		u.Redirect(w, r, audTagURL(id))
		return nil
	}
	u.Flash(r, FlashSuccess, "Tag "+t.Name+" deleted.")
	u.Redirect(w, r, Prefix+"/tags")
	return nil
}
