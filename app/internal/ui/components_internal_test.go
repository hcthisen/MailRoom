package ui

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"mailroom/internal/api"
	"mailroom/internal/core"
)

func newTestUI(t *testing.T) *UI {
	t.Helper()
	return New(Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

// serve runs h through the UI's base middleware.
func serve(u *UI, r *http.Request, h HandlerFunc) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	u.base(Action{}, h).ServeHTTP(rec, r)
	return rec
}

func TestFormFail(t *testing.T) {
	cases := []struct {
		err     error
		status  int
		field   string
		message string
	}{
		{core.Invalid("email", "is not a valid email address"), 422, "email", "Please correct the highlighted field."},
		{core.Invalid("body.subject", "is required"), 422, "subject", "Please correct the highlighted field."},
		{core.Invalid("attributes.x", "is too long"), 422, "attributes.x", "attributes.x: is too long"},
		{core.Invalid("", "nothing to change"), 422, "", "nothing to change"},
		{fmt.Errorf("contact: %w", core.ErrConflict), 409, "", "contact: conflict"},
		{fmt.Errorf("refused: %w", core.ErrForbidden), 403, "", "refused: forbidden"},
		{fmt.Errorf("v2: %w", core.ErrPrecondition), 412, "", "Someone else changed this"},
		{fmt.Errorf("x: %w", core.ErrNotFound), 0, "", ""},
		{fmt.Errorf("disk on fire"), 0, "", ""},
	}
	for _, c := range cases {
		f := EmptyForm().Set("email", "x").Set("subject", "")
		if got := f.Fail(c.err); got != c.status {
			t.Errorf("%v: status %d, want %d", c.err, got, c.status)
			continue
		}
		if c.field != "" && f.Err(c.field) == "" {
			t.Errorf("%v: no error on %q: %+v", c.err, c.field, f.Errors)
		}
		if !strings.HasPrefix(f.Message, c.message) {
			t.Errorf("%v: message %q, want %q", c.err, f.Message, c.message)
		}
	}
}

func TestParseTags(t *testing.T) {
	got := ParseTags(" news, Customer ,customer,\n vip ,, ")
	if want := []string{"news", "Customer", "vip"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseTags = %q, want %q", got, want)
	}
	if ParseTags("  ") != nil {
		t.Fatal("blank input should give no tags")
	}
}

func TestPaginate(t *testing.T) {
	type item struct{ ID int64 }
	r := httptest.NewRequest(http.MethodGet, "/admin/contacts?status=active&limit=2", nil)
	p, err := ParseList(r)
	if err != nil || p.Limit != 2 || p.After != 0 || p.Fetch() != 3 {
		t.Fatalf("ParseList = %+v, %v", p, err)
	}
	items, pg := Paginate(r, p, []item{{9}, {8}, {7}}, func(i item) int64 { return i.ID })
	if len(items) != 2 || pg.Count != 2 || pg.FirstURL != "" {
		t.Fatalf("page 1: %v %+v", items, pg)
	}
	next, _ := url.Parse(pg.NextURL)
	if next.Path != "/admin/contacts" || next.Query().Get("status") != "active" || next.Query().Get("cursor") != api.EncodeCursor(8) {
		t.Fatalf("next URL %q", pg.NextURL)
	}

	r2 := httptest.NewRequest(http.MethodGet, pg.NextURL, nil)
	p2, err := ParseList(r2)
	if err != nil || p2.After != 8 {
		t.Fatalf("page 2 params %+v, %v", p2, err)
	}
	items, pg = Paginate(r2, p2, []item{{7}}, func(i item) int64 { return i.ID })
	if len(items) != 1 || pg.NextURL != "" || pg.FirstURL != "/admin/contacts?limit=2&status=active" {
		t.Fatalf("page 2: %v %+v", items, pg)
	}

	if _, err := ParseList(httptest.NewRequest(http.MethodGet, "/admin/x?cursor=bogus", nil)); err == nil {
		t.Fatal("a bogus cursor was accepted")
	}
	if p, _ := ParseList(httptest.NewRequest(http.MethodGet, "/admin/x?limit=100000", nil)); p.Limit != MaxPageSize {
		t.Fatalf("limit not capped: %d", p.Limit)
	}
}

func TestFormatting(t *testing.T) {
	for in, want := range map[any]string{0: "0", 999: "999", 1000: "1,000", int64(-1234567): "-1,234,567", 14.0: "14"} {
		if got := formatNumber(in); got != want {
			t.Errorf("formatNumber(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[float64]string{0: "0%", 0.0025: "0.25%", 0.5: "50%", 1.0 / 3: "33.33%"} {
		if got := formatPercent(in); got != want {
			t.Errorf("formatPercent(%v) = %q, want %q", in, got, want)
		}
	}
	if got := string(Badge("contact", "suppressed_manual")); got != `<span class="badge badge-warn">suppressed</span>` {
		t.Errorf("badge = %s", got)
	}
	if got := string(Badge("", "<b>")); got != `<span class="badge badge-muted">&lt;b&gt;</span>` {
		t.Errorf("badge escaping = %s", got)
	}
	if got := string(Badge("broadcast", "sent")); !strings.Contains(got, "badge-ok") {
		t.Errorf("broadcast badge = %s", got)
	}
}

func TestRenderConfirm(t *testing.T) {
	u := newTestUI(t)
	r := httptest.NewRequest(http.MethodPost, "/admin/contacts/7/erase", nil)
	rec := serve(u, r, func(w http.ResponseWriter, r *http.Request) error {
		return u.RenderConfirm(w, r, Confirm{
			Title: "Erase this contact?", Message: "All data about <a@example.org> is deleted.",
			Details: []string{"3 messages", "1 enrollment"}, Action: "/admin/contacts/7/erase",
			Fields:       url.Values{"reason": {"asked"}, CSRFField: {"old"}, confirmField: {"no"}},
			ConfirmLabel: "Erase", Danger: true, CancelURL: "/admin/contacts/7",
		})
	})
	body := rec.Body.String()
	for _, want := range []string{"Erase this contact?", "&lt;a@example.org&gt;", "<li>3 messages</li>", `name="reason" value="asked"`,
		`name="confirm" value="yes"`, "btn-danger", `href="/admin/contacts/7"`} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `value="old"`) || strings.Contains(body, `value="no"`) {
		t.Error("the old csrf_token / confirm values were re-posted")
	}
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("confirm=yes"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if !Confirmed(req) {
		t.Error("Confirmed = false")
	}
}

func TestFieldComponent(t *testing.T) {
	u := newTestUI(t)
	f := EmptyForm().Set("email", `x"><script>`)
	f.AddError("email", "is not a valid email address")
	var b strings.Builder
	if err := u.pages["errors/error"].t.ExecuteTemplate(&b, "field", map[string]any{"Form": f, "Name": "email", "Label": "Email", "Type": "email"}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{`<label for="f-email">Email</label>`, `aria-invalid="true"`, `aria-describedby="f-email-error"`,
		`<p class="error-text" id="f-email-error">is not a valid email address</p>`, `value="x&#34;&gt;&lt;script&gt;"`} {
		if !strings.Contains(out, want) {
			t.Errorf("field lacks %q:\n%s", want, out)
		}
	}
	b.Reset()
	if err := u.pages["errors/error"].t.ExecuteTemplate(&b, "field", map[string]any{"Form": f, "Name": "password", "Label": "Password", "Type": "password"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "value=") {
		t.Errorf("a password field echoes its value: %s", b.String())
	}
}

func TestFlashAcrossRedirect(t *testing.T) {
	u := newTestUI(t)
	u.d.Config.Secret = []byte("0123456789abcdef0123456789abcdef")
	rec := serve(u, httptest.NewRequest(http.MethodPost, "/admin/x", nil), func(w http.ResponseWriter, r *http.Request) error {
		u.Flash(r, FlashSuccess, "Saved <ok>")
		u.Redirect(w, r, "/admin/y")
		return nil
	})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/y" {
		t.Fatalf("redirect %d %v", rec.Code, rec.Header())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != flashCookie {
		t.Fatalf("cookies %v", cookies)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/y", nil)
	req.AddCookie(cookies[0])
	var got []Flash
	serve(u, req, func(w http.ResponseWriter, r *http.Request) error {
		got = u.takeFlashes(w, r)
		return nil
	})
	if len(got) != 1 || got[0] != (Flash{FlashSuccess, "Saved <ok>"}) {
		t.Fatalf("flashes = %+v", got)
	}

	// htmx: HX-Redirect instead of a 303.
	hx := httptest.NewRequest(http.MethodPost, "/admin/x", nil)
	hx.Header.Set("HX-Request", "true")
	rec = serve(u, hx, func(w http.ResponseWriter, r *http.Request) error {
		u.Redirect(w, r, "/admin/z")
		return nil
	})
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "/admin/z" {
		t.Fatalf("htmx redirect %d %v", rec.Code, rec.Header())
	}
}

func TestErrorRendering(t *testing.T) {
	u := newTestUI(t)
	cases := []struct {
		err    error
		status int
		text   string
	}{
		{fmt.Errorf("contact 9: %w", core.ErrNotFound), 404, "contact 9: not found"},
		{core.Invalid("email", "is bad"), 422, "email: is bad"},
		{fmt.Errorf("secret db path /x: boom"), 500, "Something went wrong"},
	}
	for _, c := range cases {
		rec := serve(u, httptest.NewRequest(http.MethodGet, "/admin/x", nil), func(http.ResponseWriter, *http.Request) error { return c.err })
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.text) {
			t.Errorf("%v: %d\n%s", c.err, rec.Code, rec.Body.String())
		}
		if c.status == 500 && strings.Contains(rec.Body.String(), "boom") {
			t.Error("an internal error message reached the page")
		}
	}
	// htmx: an out-of-band error flash, nothing swapped.
	hx := httptest.NewRequest(http.MethodPost, "/admin/x", nil)
	hx.Header.Set("HX-Request", "true")
	rec := serve(u, hx, func(http.ResponseWriter, *http.Request) error { return fmt.Errorf("x: %w", core.ErrConflict) })
	if rec.Code != 200 || rec.Header().Get("HX-Reswap") != "none" ||
		!strings.Contains(rec.Body.String(), `hx-swap-oob="beforeend:#flashes"`) || !strings.Contains(rec.Body.String(), "x: conflict") {
		t.Fatalf("htmx error: %d %v\n%s", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestActionValidation(t *testing.T) {
	u := newTestUI(t)
	for name, a := range map[string]Action{
		"no operation": {Method: http.MethodGet, Path: Prefix + "/x1", Handler: func(http.ResponseWriter, *http.Request) error { return nil }},
		"outside":      {Method: http.MethodGet, Path: "/x2", OperationID: "list-contacts", Handler: func(http.ResponseWriter, *http.Request) error { return nil }},
		"no handler":   {Method: http.MethodGet, Path: Prefix + "/x3", OperationID: "list-contacts"},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: handle did not panic", name)
				}
			}()
			u.handle(a)
		}()
	}
}
