package ui

import (
	"errors"
	"html/template"
	"net/http"

	"mailroom/internal/contacts"
	"mailroom/internal/core"
)

type audSuppressionsData struct {
	Tabs       template.HTML
	Filter     *Form // reason, email (looked up by its hash)
	Items      []contacts.Suppression
	Pager      Pager
	AddForm    *Form
	RemoveForm *Form
	Reasons    []Option
}

// audSuppressionsList lists suppressed addresses (list-suppressions) with
// the add and remove forms. The list holds hashes only; one address can
// be looked up by entering it.
func (u *UI) audSuppressionsList(w http.ResponseWriter, r *http.Request) error {
	return u.audRenderSuppressions(w, r, http.StatusOK, nil, nil)
}

func (u *UI) audRenderSuppressions(w http.ResponseWriter, r *http.Request, status int, add, remove *Form) error {
	q := r.URL.Query()
	filter := &Form{Values: q, Errors: map[string]string{}}
	if add == nil {
		add = EmptyForm()
	}
	if remove == nil {
		remove = EmptyForm()
	}
	d := audSuppressionsData{Tabs: audTabs("suppressions"), Filter: filter, AddForm: add, RemoveForm: remove,
		Reasons: []Option{{"", "Any reason"}, {"manual", "Manual"}, {"erased", "Erased (GDPR)"}}}
	p, err := ParseList(r)
	if err != nil {
		return err
	}
	sf := contacts.SuppressionFilter{Reason: filter.Get("reason"), BeforeID: p.After, Limit: p.Fetch()}
	if e := filter.Get("email"); e != "" {
		norm, err := core.NormalizeEmail(e)
		if err != nil {
			filter.AddError("email", "is not a valid email address")
			filter.AddError("", "The address to look up is not valid.")
			if status == http.StatusOK {
				status = http.StatusUnprocessableEntity
			}
		} else {
			sf.Hash = core.EmailHash(norm)
		}
	}
	if !filter.HasErrors() {
		items, err := u.d.Contacts.ListSuppressions(r.Context(), sf)
		if err != nil {
			if st := filter.Fail(audQueryField(err)); st == 0 {
				return err
			} else if status == http.StatusOK {
				status = st
			}
		}
		d.Items, d.Pager = Paginate(r, p, items, func(s contacts.Suppression) int64 { return s.ID })
	}
	return u.PageStatus(w, r, status, "audience/suppressions", View{Title: "Suppressions", Nav: "contacts",
		Subtitle: "Addresses that are never emailed, imported or signed up again.", Data: d})
}

// audSuppressionAdd suppresses an address (add-suppression).
func (u *UI) audSuppressionAdd(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	res, err := u.d.Contacts.AddSuppression(r.Context(), f.Get("email"), f.Get("note"))
	if err == nil {
		msg := "The address is suppressed."
		switch {
		case !res.Created:
			msg = "The address was already suppressed."
		case res.StatusChanged:
			msg = "The address is suppressed, and its contact is set to suppressed."
		}
		u.Flash(r, FlashSuccess, msg)
		u.Redirect(w, r, Prefix+"/suppressions")
		return nil
	}
	if st := f.Fail(err); st != 0 {
		return u.audRenderSuppressions(w, r, st, f, nil)
	}
	return err
}

// audSuppressionRemove removes a manual suppression (remove-suppression)
// after a confirmation: the address can be emailed again.
func (u *UI) audSuppressionRemove(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	email := f.Get("remove_email")
	if !Confirmed(r) {
		if _, err := core.NormalizeEmail(email); err != nil {
			f.AddError("remove_email", "is not a valid email address")
			return u.audRenderSuppressions(w, r, http.StatusUnprocessableEntity, nil, f)
		}
		return u.RenderConfirm(w, r, Confirm{
			Title:   "Remove the suppression for " + email + "?",
			Message: "The address can be emailed, imported and signed up again. A contact with status suppressed becomes active again (or unsubscribed, if it asked to unsubscribe meanwhile).",
			Action:  Prefix + "/suppressions/remove", Fields: f.Values, ConfirmLabel: "Remove suppression",
			CancelURL: Prefix + "/suppressions", Danger: true, Nav: "contacts",
			Crumbs: []Crumb{{"Suppressions", Prefix + "/suppressions"}},
		})
	}
	res, err := u.d.Contacts.RemoveSuppression(r.Context(), email, f.Get("remove_note"))
	if err == nil {
		msg := "The suppression was removed."
		if res.StatusChanged {
			msg += " Its contact is now " + audStatusLabel(string(res.Status)) + "."
		}
		u.Flash(r, FlashSuccess, msg)
		u.Redirect(w, r, Prefix+"/suppressions")
		return nil
	}
	err = audRenameField(err, "email", "remove_email")
	if errors.Is(err, core.ErrNotFound) {
		f.AddError("remove_email", "this address is not suppressed")
		return u.audRenderSuppressions(w, r, http.StatusNotFound, nil, f)
	}
	if st := f.Fail(audRenameField(err, "note", "remove_note")); st != 0 {
		return u.audRenderSuppressions(w, r, st, nil, f)
	}
	return err
}
