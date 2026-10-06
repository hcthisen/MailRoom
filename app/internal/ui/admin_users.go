package ui

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"mailroom/internal/auth"
	"mailroom/internal/core"
)

type admUsersData struct {
	Tabs  admTabList
	Items []auth.User
	Pager Pager
	Me    int64
	Form  *Form // the create form
}

func admUsersCrumbs() []Crumb {
	return []Crumb{{Label: "Settings", URL: Prefix + "/settings"}, {Label: "Users", URL: Prefix + "/users"}}
}

// admUsersView lists the users (list-users) with the create form f.
func (u *UI) admUsersView(r *http.Request, f *Form) (View, error) {
	p, err := ParseList(r)
	if err != nil {
		return View{}, err
	}
	list, err := u.d.Auth.ListUsers(r.Context(), p.After, p.Fetch())
	if err != nil {
		return View{}, err
	}
	items, pager := Paginate(r, p, list, func(x auth.User) int64 { return x.ID })
	me := int64(0)
	if s := SessionFrom(r); s != nil {
		me = s.UserID
	}
	return View{Title: "Users", Nav: "settings",
		Subtitle: "People who can sign in to this admin. All users have the same permissions.",
		Data:     admUsersData{Tabs: admTabs(admSettingsSection, Prefix+"/users"), Items: items, Pager: pager, Me: me, Form: f}}, nil
}

// admUsersList is the user list (list-users).
func (u *UI) admUsersList(w http.ResponseWriter, r *http.Request) error {
	v, err := u.admUsersView(r, EmptyForm())
	if err != nil {
		return err
	}
	return u.Page(w, r, "admin/users", v)
}

type admUserCreatedData struct {
	User  auth.User
	Field admSecret
}

// admUserCreate adds a user (create-user). A generated password is shown
// once, on the POST response itself.
func (u *UI) admUserCreate(w http.ResponseWriter, r *http.Request) error {
	f := NewForm(r)
	usr, generated, err := u.d.Auth.CreateUser(r.Context(), auth.CreateUserInput{
		Email: f.Get("email"), Name: f.Get("name"), Password: r.PostFormValue("password"),
	})
	if err != nil {
		st := f.Fail(err)
		if st == 0 {
			return err
		}
		v, verr := u.admUsersView(r, f)
		if verr != nil {
			return verr
		}
		return u.PageStatus(w, r, st, "admin/users", v)
	}
	if generated != "" {
		return u.Page(w, r, "admin/user_created", View{Title: "User added", Nav: "settings", Crumbs: admUsersCrumbs(),
			Data: admUserCreatedData{User: usr, Field: admSecret{ID: "user-password", Label: "Generated password", Value: generated}}})
	}
	u.Flash(r, FlashSuccess, fmt.Sprintf("%s can now sign in with the password you chose.", usr.Email))
	u.Redirect(w, r, Prefix+"/users")
	return nil
}

type admUserData struct {
	User auth.User
	Self bool
	Form *Form
}

func admUserForm(usr auth.User) *Form {
	return EmptyForm().Set("email", usr.Email).Set("name", usr.Name).Set("version", strconv.FormatInt(usr.Version, 10))
}

func (u *UI) admUserView(r *http.Request, usr auth.User, f *Form) View {
	title := usr.Email
	if usr.Name != "" {
		title = usr.Name
	}
	return View{Title: title, Subtitle: usr.Email, Nav: "settings", Crumbs: admUsersCrumbs(),
		Data: admUserData{User: usr, Self: admIsSelf(r, usr.ID), Form: f}}
}

func admIsSelf(r *http.Request, id int64) bool {
	s := SessionFrom(r)
	return s != nil && s.UserID == id
}

// admUserPage shows a user with the edit form (get-user).
func (u *UI) admUserPage(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	usr, err := u.d.Auth.GetUser(r.Context(), id)
	if err != nil {
		return err
	}
	return u.Page(w, r, "admin/user", u.admUserView(r, usr, admUserForm(usr)))
}

// admFormVersion reads the hidden version field (the If-Match of the
// update, delete and TOTP reset).
func admFormVersion(f *Form) (int64, error) {
	v, err := strconv.ParseInt(f.Get("version"), 10, 64)
	if err != nil || v < 1 {
		return 0, fieldError("", "the form is incomplete: reload the page and try again")
	}
	return v, nil
}

// admSignedOut ends the request of a user whose own sessions the service
// just ended (new password, email or TOTP): back to the sign-in page.
func (u *UI) admSignedOut(w http.ResponseWriter, r *http.Request, msg string) {
	auth.ClearSessionCookie(w, u.secure)
	u.Flash(r, FlashSuccess, msg)
	u.Redirect(w, r, Prefix+"/login")
}

// admUserUpdate saves the edit form (update-user) with the version it was
// loaded with as If-Match.
func (u *UI) admUserUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	cur, err := u.d.Auth.GetUser(ctx, id)
	if err != nil {
		return err
	}
	f := NewForm(r)
	email, name := f.Get("email"), f.Get("name")
	in := auth.UpdateUserInput{Email: &email, Name: &name}
	pw := r.PostFormValue("password")
	if pw != "" {
		in.Password = &pw
	}
	version, err := admFormVersion(f)
	var usr auth.User
	if err == nil {
		usr, err = u.d.Auth.UpdateUser(ctx, id, in, version)
	}
	if err != nil {
		st := f.Fail(err)
		if st == 0 {
			return err
		}
		if st == http.StatusPreconditionFailed {
			f.Message = "Someone else changed this user while you were editing. Nothing was saved. " +
				"Reload the page to see the current details, then make your change again."
		}
		return u.PageStatus(w, r, st, "admin/user", u.admUserView(r, cur, f))
	}
	admSignedOut := pw != "" || usr.Email != cur.Email
	if admIsSelf(r, id) && admSignedOut {
		u.admSignedOut(w, r, "Your account was updated and you were signed out everywhere. Sign in again with your new details.")
		return nil
	}
	msg := "User saved."
	if admSignedOut {
		msg = "User saved. They were signed out everywhere and sign in again with the new details."
	}
	u.Flash(r, FlashSuccess, msg)
	u.Redirect(w, r, fmt.Sprintf("%s/users/%d", Prefix, id))
	return nil
}

// admUserDelete deletes a user (delete-user) after a confirmation.
func (u *UI) admUserDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	usr, err := u.d.Auth.GetUser(ctx, id)
	if err != nil {
		return err
	}
	f := NewForm(r)
	self := admIsSelf(r, id)
	if !Confirmed(r) {
		details := []string{"Email: " + usr.Email}
		if usr.LastLoginAt != nil {
			details = append(details, "Last signed in "+u.ago(usr.LastLoginAt))
		}
		if self {
			details = append(details, "This is your own account: you are signed out and can't sign in again.")
		}
		toks, err := u.d.Auth.UserTokens(ctx, id)
		if err != nil {
			return err
		}
		for _, t := range toks {
			details = append(details, fmt.Sprintf("API token %q (%s scope) they created is revoked too", t.Name, t.Scope))
		}
		return u.RenderConfirm(w, r, Confirm{
			Title:   "Delete this user?",
			Message: "The user can no longer sign in, their sessions end now, and the API tokens they created are revoked. The audit log keeps what they did. This can't be undone.",
			Details: details, Action: fmt.Sprintf("%s/users/%d/delete", Prefix, id),
			Fields:       url.Values{"version": {strconv.FormatInt(usr.Version, 10)}},
			ConfirmLabel: "Delete user", CancelURL: fmt.Sprintf("%s/users/%d", Prefix, id), Danger: true,
			Nav: "settings", Crumbs: admUsersCrumbs(),
		})
	}
	version, err := admFormVersion(f)
	if err == nil {
		err = u.d.Auth.DeleteUser(ctx, id, version)
	}
	if err != nil {
		if st := f.Fail(err); st != 0 {
			u.Flash(r, FlashError, "The user was not deleted: "+f.Message)
			u.Redirect(w, r, fmt.Sprintf("%s/users/%d", Prefix, id))
			return nil
		}
		return err
	}
	if self {
		u.admSignedOut(w, r, "Your account was deleted.")
		return nil
	}
	u.Flash(r, FlashSuccess, fmt.Sprintf("The user %s was deleted.", usr.Email))
	u.Redirect(w, r, Prefix+"/users")
	return nil
}

// admErrOwnTOTP refuses TOTP enrolment for another user: the secret has to
// go into that person's own authenticator app.
func admErrOwnTOTP(email string) error {
	return admRefusal("Only " + email + " can set up two-factor sign-in for their account, after signing in themselves.")
}

// admRefusal is a core.ErrForbidden with a message meant for people.
type admRefusal string

func (e admRefusal) Error() string { return string(e) }
func (e admRefusal) Unwrap() error { return core.ErrForbidden }

type admTotpData struct {
	User     auth.User
	Secret   string
	URI      string
	Form     *Form
	KeyField admSecret
	URIField admSecret
	QR       template.HTML // the otpauth link as a QR code ("" if it can't be drawn)
}

func admGroupSecret(s string) string {
	var b strings.Builder
	for i, c := range s {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func (u *UI) admTotpView(usr auth.User, secret, uri string, f *Form) View {
	var qr template.HTML
	if strings.HasPrefix(uri, "otpauth://") {
		qr, _ = qrSVG(uri, "QR code to scan with your authenticator app")
	}
	return View{Title: "Set up two-factor sign-in", Nav: "settings",
		Crumbs: append(admUsersCrumbs(), Crumb{Label: usr.Email, URL: fmt.Sprintf("%s/users/%d", Prefix, usr.ID)}),
		Data: admTotpData{User: usr, Secret: secret, URI: uri, Form: f,
			KeyField: admSecret{ID: "totp-secret", Label: "Key", Value: admGroupSecret(secret)},
			URIField: admSecret{ID: "totp-uri", Label: "otpauth link", Value: uri}, QR: qr}}
}

// admUserTOTPBegin starts TOTP enrolment for the signed-in user
// (begin-user-totp): a new secret to add to an authenticator app. Nothing
// changes until the code is confirmed.
func (u *UI) admUserTOTPBegin(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	usr, err := u.d.Auth.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if !admIsSelf(r, id) {
		return admErrOwnTOTP(usr.Email)
	}
	e, err := u.d.Auth.BeginTOTP(ctx, id)
	if err != nil {
		return err
	}
	return u.Page(w, r, "admin/totp", u.admTotpView(usr, e.Secret, e.OtpauthURI, EmptyForm()))
}

// admUserTOTPConfirm turns TOTP on with a valid code (confirm-user-totp). The
// service ends the user's sessions, so they sign in again with a code.
func (u *UI) admUserTOTPConfirm(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	usr, err := u.d.Auth.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if !admIsSelf(r, id) {
		return admErrOwnTOTP(usr.Email)
	}
	f := NewForm(r)
	secret := f.Get("secret")
	code := strings.ReplaceAll(f.Get("code"), " ", "")
	if _, err := u.d.Auth.ConfirmTOTP(ctx, id, secret, code); err != nil {
		st := f.Fail(err)
		if st == 0 {
			return err
		}
		return u.PageStatus(w, r, st, "admin/totp", u.admTotpView(usr, secret, f.Get("otpauth_uri"), f))
	}
	u.admSignedOut(w, r, "Two-factor sign-in is on. Sign in again with your password and a code from your authenticator app.")
	return nil
}

// admUserTOTPReset turns TOTP off for a user (reset-user-totp) after a
// confirmation, e.g. after a lost phone.
func (u *UI) admUserTOTPReset(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	usr, err := u.d.Auth.GetUser(ctx, id)
	if err != nil {
		return err
	}
	self := admIsSelf(r, id)
	back := fmt.Sprintf("%s/users/%d", Prefix, id)
	if !usr.TOTPEnabled {
		u.Flash(r, FlashInfo, "Two-factor sign-in is already off for "+usr.Email+".")
		u.Redirect(w, r, back)
		return nil
	}
	f := NewForm(r)
	if !Confirmed(r) {
		details := []string{"User: " + usr.Email, "They are signed out everywhere and sign in with the password only."}
		if self {
			details[1] = "This is your own account: you are signed out and sign in again with the password only."
		}
		return u.RenderConfirm(w, r, Confirm{
			Title:   "Turn off two-factor sign-in?",
			Message: "Use this when the authenticator app is lost. Two-factor sign-in can be set up again afterwards.",
			Details: details, Action: back + "/totp/reset",
			Fields:       url.Values{"version": {strconv.FormatInt(usr.Version, 10)}},
			ConfirmLabel: "Turn off two-factor sign-in", CancelURL: back, Danger: true, Nav: "settings", Crumbs: admUsersCrumbs(),
		})
	}
	version, err := admFormVersion(f)
	if err == nil {
		_, err = u.d.Auth.ResetTOTP(ctx, id, version)
	}
	if err != nil {
		if st := f.Fail(err); st != 0 {
			u.Flash(r, FlashError, "Two-factor sign-in was not turned off: "+f.Message)
			u.Redirect(w, r, back)
			return nil
		}
		return err
	}
	if self {
		u.admSignedOut(w, r, "Two-factor sign-in is off for your account. Sign in again with your password.")
		return nil
	}
	u.Flash(r, FlashSuccess, "Two-factor sign-in is off for "+usr.Email+". They were signed out everywhere.")
	u.Redirect(w, r, back)
	return nil
}
