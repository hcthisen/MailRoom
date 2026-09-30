package ui

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"mailroom/internal/api"
	"mailroom/internal/auth"
)

// admTokenRow is one row of the token list.
type admTokenRow struct {
	auth.Token
	State string // active, expired or revoked
}

type admTokensData struct {
	Items          []admTokenRow
	Pager          Pager
	IncludeRevoked bool
	Form           *Form // the create form
	Scopes         []Option
}

var admTokenScopes = []Option{
	{string(api.ScopeRead), "read: may only read (GET requests)"},
	{string(api.ScopeAdmin), "admin: may read and change everything"},
}

func (u *UI) admTokenState(t auth.Token) string {
	switch {
	case t.RevokedAt != nil:
		return "revoked"
	case t.ExpiresAt != nil && !t.ExpiresAt.After(u.clock.Now()):
		return "expired"
	}
	return "active"
}

// admTokensView lists the tokens (list-tokens) with the create form f.
func (u *UI) admTokensView(r *http.Request, f *Form) (View, error) {
	p, err := ParseList(r)
	if err != nil {
		return View{}, err
	}
	revoked := r.URL.Query().Get("revoked") == "1"
	list, err := u.d.Auth.ListTokens(r.Context(), p.After, p.Fetch(), revoked)
	if err != nil {
		return View{}, err
	}
	items, pager := Paginate(r, p, list, func(t auth.Token) int64 { return t.ID })
	d := admTokensData{Pager: pager, IncludeRevoked: revoked, Form: f, Scopes: admTokenScopes}
	for _, t := range items {
		d.Items = append(d.Items, admTokenRow{Token: t, State: u.admTokenState(t)})
	}
	return View{Title: "API tokens", Nav: "tokens",
		Subtitle: "Tokens let integrations and coding agents use the REST API (Authorization: Bearer …).", Data: d}, nil
}

// admTokensList is the token page (list-tokens).
func (u *UI) admTokensList(w http.ResponseWriter, r *http.Request) error {
	f := EmptyForm().Set("scope", string(api.ScopeRead))
	v, err := u.admTokensView(r, f)
	if err != nil {
		return err
	}
	return u.Page(w, r, "admin/tokens", v)
}

type admTokenCreatedData struct {
	Token auth.Token
	Field admSecret
}

// admTokenCreate creates a token (create-token) and shows its secret once.
// The page is the POST response itself: the secret never goes into a
// cookie, a URL or a later page.
func (u *UI) admTokenCreate(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	in := auth.TokenInput{Name: f.Get("name"), Scope: api.Scope(f.Get("scope"))}
	var err error
	if s := f.Get("expires_at"); s != "" {
		var t time.Time
		if t, err = u.ParseInputTime("expires_at", s); err == nil {
			in.ExpiresAt = &t
		}
	}
	var tok auth.Token
	var secret string
	if err == nil {
		tok, secret, err = u.d.Auth.CreateToken(r.Context(), in)
	}
	if err != nil {
		st := f.Fail(err)
		if st == 0 {
			return err
		}
		v, verr := u.admTokensView(r, f)
		if verr != nil {
			return verr
		}
		return u.PageStatus(w, r, st, "admin/tokens", v)
	}
	return u.Page(w, r, "admin/token_created", View{Title: "Token created", Nav: "tokens",
		Crumbs: []Crumb{{Label: "API tokens", URL: Prefix + "/tokens"}},
		Data:   admTokenCreatedData{Token: tok, Field: admSecret{ID: "token-secret", Label: fmt.Sprintf("Token %q", tok.Name), Value: secret}}})
}

// admTokenRevoke revokes a token (revoke-token) after a confirmation that
// names it.
func (u *UI) admTokenRevoke(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	t, err := u.d.Auth.GetToken(ctx, id)
	if err != nil {
		return err
	}
	if t.RevokedAt != nil {
		u.Flash(r, FlashInfo, fmt.Sprintf("The token %q was already revoked.", t.Name))
		u.Redirect(w, r, Prefix+"/tokens")
		return nil
	}
	if !Confirmed(r) {
		details := []string{
			"Name: " + t.Name,
			"Scope: " + string(t.Scope),
			"Starts with: " + t.Prefix,
		}
		if t.LastUsedAt != nil {
			details = append(details, "Last used "+u.ago(t.LastUsedAt)+" ("+u.fmtTime(t.LastUsedAt)+")")
		} else {
			details = append(details, "Never used")
		}
		return u.RenderConfirm(w, r, Confirm{
			Title: "Revoke this token?",
			Message: "Anything that uses this token (an integration, a script or a coding agent) stops working immediately. " +
				"This can't be undone, and the name can't be used for a new token.",
			Details: details, Action: fmt.Sprintf("%s/tokens/%d/revoke", Prefix, t.ID), Fields: url.Values{},
			ConfirmLabel: "Revoke token", CancelURL: Prefix + "/tokens", Danger: true, Nav: "tokens",
			Crumbs: []Crumb{{Label: "API tokens", URL: Prefix + "/tokens"}},
		})
	}
	if err := u.d.Auth.RevokeToken(ctx, id); err != nil {
		return err
	}
	u.Flash(r, FlashSuccess, fmt.Sprintf("The token %q is revoked.", t.Name))
	u.Redirect(w, r, Prefix+"/tokens")
	return nil
}
