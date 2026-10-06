package content_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/settings"
	"mailroom/internal/testutil"
	"mailroom/internal/tokens"
)

const testBaseURL = "https://newsletter.example.com"

type fixture struct {
	t        *testing.T
	ctx      context.Context
	d        *db.DB
	clock    *testutil.Clock
	settings *settings.Service
	signer   *tokens.Signer
	svc      *content.Service
	dataDir  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := testutil.NewDB(t)
	clock := testutil.NewClock(time.Time{})
	st := settings.New(settings.Deps{DB: d, Clock: clock, Domain: "example.com"})
	signer := testutil.NewSigner(t, clock)
	dir := t.TempDir()
	svc := content.New(content.Deps{
		DB: d, Settings: st, Signer: signer, BaseURL: testBaseURL + "/", DataDir: dir, Clock: clock,
	})
	f := &fixture{t: t, ctx: context.Background(), d: d, clock: clock, settings: st, signer: signer, svc: svc, dataDir: dir}
	footer := "Example Ltd\n1 High Street, London"
	owner := "owner@example.org"
	name := "Example News"
	f.patchSettings(settings.Patch{CompanyFooter: &footer, OwnerEmail: &owner, Sender: &settings.SenderPatch{FromName: &name}})
	return f
}

func (f *fixture) patchSettings(p settings.Patch) {
	f.t.Helper()
	if _, err := f.settings.Update(f.ctx, p, 0); err != nil {
		f.t.Fatalf("update settings: %v", err)
	}
}

func (f *fixture) tracking(clicks, opens bool) {
	f.t.Helper()
	f.patchSettings(settings.Patch{Tracking: &settings.TrackingPatch{Clicks: &clicks, Opens: &opens}})
}

func (f *fixture) tx(fn func(tx *sql.Tx) error) error {
	return f.d.WithTx(f.ctx, fn)
}

func (f *fixture) createEmail(in content.EmailInput) content.Email {
	f.t.Helper()
	var e content.Email
	if err := f.tx(func(tx *sql.Tx) error {
		var err error
		e, err = f.svc.CreateEmail(f.ctx, tx, in)
		return err
	}); err != nil {
		f.t.Fatalf("create email: %v", err)
	}
	return e
}

func (f *fixture) putLayout(in content.LayoutInput) content.Layout {
	f.t.Helper()
	var l content.Layout
	if err := f.tx(func(tx *sql.Tx) error {
		var err error
		l, err = f.svc.PutLayout(f.ctx, tx, in, 0)
		return err
	}); err != nil {
		f.t.Fatalf("put layout: %v", err)
	}
	return l
}

func (f *fixture) contact(email, first string, attrs map[string]any) core.Contact {
	f.t.Helper()
	c := testutil.InsertContact(f.t, f.d, email, core.StatusActive)
	attrJSON, err := db.MarshalJSON(attrs)
	if err != nil {
		f.t.Fatal(err)
	}
	testutil.Exec(f.t, f.d, `UPDATE contacts SET first_name = ?, attributes = ? WHERE id = ?`, first, attrJSON, c.ID)
	c.FirstName, c.Attributes = first, attrs
	return c
}

// fieldOf returns the field of a validation error, or "!" when err isn't one.
func fieldOf(err error) string {
	var ve *core.ValidationError
	if errors.As(err, &ve) {
		return ve.Field
	}
	return "!"
}
