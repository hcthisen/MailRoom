package sending

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"mailroom/internal/config"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/outbox"
	"mailroom/internal/settings"
	"mailroom/internal/testutil"
)

const testDomain = "example.com"

// fakeRenderer renders "Subject <id>" and records its inputs.
type fakeRenderer struct {
	mu      sync.Mutex
	inputs  []core.RenderInput
	err     error
	noUnsub bool
}

func (r *fakeRenderer) Render(_ context.Context, in core.RenderInput) (core.Rendered, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inputs = append(r.inputs, in)
	if r.err != nil {
		return core.Rendered{}, r.err
	}
	out := core.Rendered{
		Subject: fmt.Sprintf("Subject %d", in.MessageID),
		HTML:    fmt.Sprintf("<p>Hello %d</p>", in.MessageID),
		Text:    fmt.Sprintf("Hello %d", in.MessageID),
	}
	if in.Marketing && !r.noUnsub {
		out.UnsubscribeURL = fmt.Sprintf("https://newsletter.example.com/u/tok%d", in.MessageID)
	}
	return out, nil
}

// input returns the last render input for message id.
func (r *fakeRenderer) input(t *testing.T, id int64) core.RenderInput {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.inputs) - 1; i >= 0; i-- {
		if r.inputs[i].MessageID == id {
			return r.inputs[i]
		}
	}
	t.Fatalf("message %d was not rendered", id)
	return core.RenderInput{}
}

func (r *fakeRenderer) last() core.RenderInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inputs[len(r.inputs)-1]
}

// fakeAccount is a scripted SES GetAccount.
type fakeAccount struct {
	mu    sync.Mutex
	out   *sesv2.GetAccountOutput
	err   error
	calls int
}

func (f *fakeAccount) GetAccount(context.Context, *sesv2.GetAccountInput, ...func(*sesv2.Options)) (*sesv2.GetAccountOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.out, f.err
}

func (f *fakeAccount) set(enabled bool, maxRate float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := "HEALTHY"
	if !enabled {
		status = "SHUTDOWN"
	}
	f.out = &sesv2.GetAccountOutput{
		SendingEnabled:          enabled,
		ProductionAccessEnabled: true,
		EnforcementStatus:       &status,
		SendQuota:               &types.SendQuota{MaxSendRate: maxRate, Max24HourSend: 50000, SentLast24Hours: 12},
	}
}

type env struct {
	t        *testing.T
	ctx      context.Context
	db       *db.DB
	clock    *testutil.Clock
	provider *testutil.FakeProvider
	renderer *fakeRenderer
	account  *fakeAccount
	settings *settings.Service
	svc      *Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testutil.NewDB(t)
	clock := testutil.NewClock(time.Time{})
	st := settings.New(settings.Deps{DB: d, Clock: clock, Domain: testDomain})
	ctx := context.Background()
	if _, err := st.SeedFromConfig(ctx, config.Config{
		Domain: testDomain, FromName: "Example News", FromEmail: "news@example.com",
		ReplyToEmail: "hello@example.com", OwnerEmail: "owner@example.org", Timezone: "UTC",
		CompanyFooter: "Example Ltd, 1 Street, Town",
	}); err != nil {
		t.Fatal(err)
	}
	e := &env{
		t: t, ctx: ctx, db: d, clock: clock, provider: testutil.NewFakeProvider(),
		renderer: &fakeRenderer{}, account: &fakeAccount{}, settings: st,
	}
	e.account.set(true, 14)
	e.svc = New(Deps{
		DB: d, Clock: clock, Settings: st, Config: config.Config{Domain: testDomain},
		Renderer: e.renderer, Provider: e.provider, Account: e.account,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Workers:      4,
		PollInterval: 5 * time.Millisecond,
	})
	// The bucket "sleeps" by advancing the fake clock.
	e.svc.bucket.sleep = func(ctx context.Context, d time.Duration) error {
		clock.Advance(d)
		return ctx.Err()
	}
	return e
}

// setRateOverride stores a fixed send rate in the settings.
func (e *env) setRateOverride(r float64) {
	e.t.Helper()
	p := settings.Patch{}
	p.Sending = &settings.SendingPatch{RateOverridePerSec: &r}
	if _, err := e.settings.Update(e.ctx, p, 0); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) enqueue(s outbox.MessageSpec) int64 {
	e.t.Helper()
	id, _, err := outbox.Enqueue(e.ctx, e.db.Write, e.clock, s)
	if err != nil {
		e.t.Fatalf("enqueue: %v", err)
	}
	return id
}

// broadcast inserts a broadcast in status and returns (broadcastID, emailID).
func (e *env) broadcast(status string) (int64, int64) {
	e.t.Helper()
	emailID := testutil.InsertEmail(e.t, e.db, "News", "Hello")
	bID := testutil.InsertBroadcast(e.t, e.db, "B", emailID)
	testutil.Exec(e.t, e.db, `UPDATE broadcasts SET status = ? WHERE id = ?`, status, bID)
	return bID, emailID
}

func (e *env) broadcastMessage(bID, emailID int64, c core.Contact) int64 {
	return e.enqueue(outbox.MessageSpec{Kind: core.KindBroadcast, BroadcastID: bID, EmailID: emailID, ContactID: c.ID, ToEmail: c.Email})
}

// sequenceMessage creates a sequence, step and enrollment for c (with the
// given enrollment status) and queues the step.
func (e *env) sequenceMessage(c core.Contact, enrollmentStatus string) int64 {
	e.t.Helper()
	emailID := testutil.InsertEmail(e.t, e.db, "Step", "Hello")
	seq := testutil.InsertSequence(e.t, e.db, fmt.Sprintf("seq-%d", c.ID))
	step := testutil.InsertStep(e.t, e.db, seq, 1, 0, emailID)
	enr := testutil.InsertEnrollment(e.t, e.db, seq, c.ID, step)
	if enrollmentStatus != "active" {
		testutil.Exec(e.t, e.db, `UPDATE enrollments SET status = ?, next_run_at = NULL WHERE id = ?`, enrollmentStatus, enr)
	}
	return e.enqueue(outbox.MessageSpec{Kind: core.KindSequence, EnrollmentID: enr, StepID: step, EmailID: emailID, ContactID: c.ID, ToEmail: c.Email})
}

func (e *env) optinMessage(c core.Contact) int64 {
	return e.enqueue(outbox.MessageSpec{Kind: core.KindOptin, SystemTemplate: core.TemplateOptinConfirm, ContactID: c.ID, ToEmail: c.Email,
		Data: map[string]any{"confirm_url": "https://newsletter.example.com/f/confirm/x"}})
}

func (e *env) testMessage(to string, contactID int64) int64 {
	e.t.Helper()
	emailID := testutil.InsertEmail(e.t, e.db, "Test", "Hello")
	return e.enqueue(outbox.MessageSpec{Kind: core.KindTest, EmailID: emailID, ContactID: contactID, ToEmail: to})
}

func (e *env) sendDue() int {
	e.t.Helper()
	n, err := e.svc.SendDue(e.ctx)
	if err != nil {
		e.t.Fatalf("SendDue: %v", err)
	}
	return n
}

// row is a message's status fields.
type row struct {
	Status, Error, Subject, SESID, Next string
	Attempts                            int
}

func (e *env) row(id int64) row {
	e.t.Helper()
	var r row
	var ses sql.NullString
	err := e.db.Read.QueryRowContext(e.ctx,
		`SELECT status, error, subject, ses_message_id, attempts, next_attempt_at FROM messages WHERE id = ?`, id).
		Scan(&r.Status, &r.Error, &r.Subject, &ses, &r.Attempts, &r.Next)
	if err != nil {
		e.t.Fatalf("row %d: %v", id, err)
	}
	r.SESID = ses.String
	return r
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	return testutil.Count(e.t, e.db.Read, q, args...)
}

func (e *env) state() State {
	e.t.Helper()
	st, err := e.svc.GetState(e.ctx, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

var errNetwork = errors.New("dial tcp: connection reset")
