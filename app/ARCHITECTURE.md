# Mailroom architecture (as built)

[PRD.md](PRD.md) says **what** Mailroom does. This file says **how the code is laid out**: the package boundaries, the shared contracts, the workers, and the decisions made during the build (§10). [README.md](README.md) covers the everyday developer tasks: build, run, test, add an operation, upgrade, migrate. A package that needs something from another package uses only the contracts listed here, never that package's internals. A change to a contract is a change to this file.

## 1. Stack

| Concern | Choice |
|---|---|
| Language | Go (module `mailroom`). The go.mod `go` line is `1.26.0`, the minimum that `golang.org/x/*` and `modernc.org/sqlite` require. It is built and tested with Go 1.27 |
| DB | SQLite via `modernc.org/sqlite` (pure Go, `CGO_ENABLED=0`), in WAL mode |
| HTTP | `net/http` with Go 1.22+ `ServeMux` patterns |
| REST API + OpenAPI 3.1 | **Huma v2** (`github.com/danielgtaylor/huma/v2` + `adapters/humago`). It is code-first: the spec is generated from the registered operations, so it can't drift. It is served at `/api/v1/openapi.json` and `.yaml` (Huma's CDN docs page is disabled). `TestSpecSnapshot` in `internal/app`, the only package that registers every operation, checks the snapshot in `api/openapi.yaml`, and `-update` rewrites it |
| Admin UI | `html/template` + htmx 2 (vendored `static/htmx.min.js`, see `static/VENDOR.md`), embedded with `embed` |
| HTTPS | `golang.org/x/crypto/acme/autocert` (built in), or `HTTP_ONLY=1` / `serve --http-only` behind a proxy |
| AWS | AWS SDK for Go v2: `sesv2`, `sns`, `iam`, `s3`, `sts` |
| Cloudflare | plain `net/http` client (`internal/provision/cloudflare.go`) |
| Markdown | `github.com/yuin/goldmark` |
| YAML | `gopkg.in/yaml.v3` |
| Passwords | argon2id (`golang.org/x/crypto/argon2`), PHC string format, t=2, m=64 MiB, p=2 |
| TOTP | own RFC 6238 implementation (`internal/auth/totp.go`): 30 s, 6 digits, ±1 step. The setup page shows the otpauth link as a QR code from an own small encoder (`internal/ui/qr.go`: byte mode, level M, versions 1-10, inline SVG) |
| HTML parsing (link rewrite, text version) | `golang.org/x/net/html` |
| Merge fields | own small engine (`internal/content/merge.go`), not a template package (§10) |

Add no other third-party modules without updating this table and `internal/deps/deps.go`. That file has the `tools` build tag and pins every module in go.mod. All modules are vendored in `vendor/` and committed (about 150 MB), so the image builds with no network access except for pulling the base images (PRD O-6).

## 2. Package layout

```text
app/
  cmd/mailroom/     main.go + registry.go: subcommand dispatch, one cmd_*.go per command:
                    serve, admin, token, doctor, provision, dns, backup, restore, migrate, version;
                    hidden: healthcheck (the container health probe)
  internal/
    config/        [F0] env config (§5), <data>/secret.key
    db/            [F0] open (write pool + read pool), embedded migrations, tx helpers, time format, VACUUM INTO backup
    core/          [F0] shared domain types, enums, errors, cross-package interfaces (§4)
    hooks/         [F0] in-process event bus (§4.3)
    tokens/        [F0] HMAC-signed public tokens (unsubscribe, click, open, optin, resubscribe)
    outbox/        [F0] Enqueue(): the ONLY way to create messages rows (§4.4)
    contactstore/  [F0] core contact operations used by everyone (§4.5)
    testutil/      [F0] temp DB, fixtures, fake clock, fake email provider (tests only)
    server/        [F0] HTTP server (autocert / http-only), security headers, request log, worker supervisor, graceful shutdown
    api/           [F1] Huma setup, error format, auth middleware, scopes, idempotency, ETag, pagination, rate limit, dry run, route registry
    auth/          [F1] users, sessions, API tokens, TOTP, CSRF, login limits + API for users/tokens/me + CLI bootstrap funcs
    audit/         [F1] audit.Log() + GET /api/v1/audit
    jobs/          [F1] job runner + GET /api/v1/jobs, /jobs/{id}
    settings/      [F1] typed settings + GET/PATCH /api/v1/settings; also a raw key/value store (PutRaw) for package state
    contacts/      [W1] contacts/tags/suppressions/status API, search, GDPR export/erase, timeline, bulk tag job
    segments/      [W1] segment filter model, SQL compiler, count/preview, API; implements core.SegmentResolver and core.TagReferrer
    content/       [W1] emails CRUD, base layout, merge engine, renderer (implements core.Renderer), tracking links, media, previews, test sends
    sending/       [W1] sender worker, global token bucket, SES provider, MIME builder, retries, final eligibility check,
                        SES account poller, sending pause state, alerts (implements core.Alerter), messages/events/clicks API
    system/        [W1] health, version, doctor, backups (local + S3), restore, retention, health-watch, SES status API
    provision/     [W1] CLI: provision ses / provision dns / dns records
    stats/         [W1] dashboard numbers: GET /api/v1/stats/dashboard (read-only)
    events/        [W2] SNS webhook, bounce/complaint/delivery processing, public /u /c /o handlers, SNS auto-subscribe
    sequences/     [W2] sequences, steps, enrollments, scheduler, tag triggers, YAML import/export; implements core.Enroller
    broadcasts/    [W2] broadcasts, snapshot job, schedule/pause/resume/cancel, circuit breaker, stats, web archive
    forms/         [W2] forms CRUD, public /f endpoints, hosted page, snippet, double opt-in, submissions, opt-in expiry
    imports/       [W2] CSV import (I-1) with dry run, as a resumable job
    app/           [W3] wiring: builds every service, registers routes/jobs/hooks/workers; end-to-end tests
    ui/            [W3] admin UI under /admin/ (htmx), action registry, UI/API parity test. Page groups:
                        <group>_*.go + templates/<group>/ (audience, broadcasts, automation, content, admin, dashboard);
                        templates/<group>/_*.html are partials parsed into every page of the group (embedded with all:templates)
    deps/          //go:build tools: pins the third-party modules (never compiled)
  static/          [W3] htmx.min.js, app.css, app.js + VENDOR.md; package static embeds them
  docs/            [W3] api.md (agent guide, served at /api/v1/docs), import-format.md, sequence-format.md; package docs embeds them
  api/openapi.yaml [W3] spec snapshot
  Dockerfile       [W3] multi-stage: golang:${GO_VERSION}-alpine (vendored modules) → gcr.io/distroless/static-debian12:nonroot, uid 65532
  scripts/         test.sh + wsl-exec.sh (tests on Windows run in WSL)
  vendor/          go mod vendor (PRD O-6); re-run after any go.mod change
```

The server-side compose file and runbook are in the kit's `../deploy/`.

The layers set the import rule. `[F0]` and `[F1]` are foundation packages. `[W1]` packages import only foundation packages. `[W2]` packages import foundation and W1 packages. `[W3]` packages may import anything. Packages on the same layer don't import each other: they cooperate through the `core` interfaces (§4.2), and `internal/app` wires them. The built import graph follows this rule (`go list -f '{{.ImportPath}}: {{.Imports}}' ./internal/...`). For example, `broadcasts` and `sequences` import `content`, `broadcasts` imports `segments`, and no W1 package imports another.

## 3. Conventions

- **Service layer:** each domain package has a `Service` struct holding its dependencies, created with `New(deps Deps) *Service`. Business logic lives in methods on it. The API handlers (`api*.go`) and the UI handlers (`internal/ui`) call the same methods. Handlers contain no SQL. The UI uses `Deps.DB` only for `db.WithTx` around service methods that take the caller's transaction, as the API handlers do.
- **SQL:** plain `database/sql`, with the SQL next to the code. No ORM. Always use parameters, never string-concatenate values. The segment compiler builds SQL from a closed set of whitelisted fragments.
- **Transactions:** use `d.WithTx(ctx, func(tx *sql.Tx) error)`, a method on `*db.DB`, which runs on the write pool with BEGIN IMMEDIATE. The write pool has **one** connection: never start a second write (a `d.WithTx`, `d.Write.Exec`, or a helper that writes on `d.Write`) while you hold a transaction, or it waits forever. Functions that must join a caller's transaction take `q db.Querier` (satisfied by `*sql.DB`, `*sql.Conn` and `*sql.Tx`) or `*sql.Tx`. This includes `jobs.Runner.Submit` and `core.Alerter.Alert`. `content.Render` may write to `links` on its own, so never call it inside a write transaction. Pass the request `ctx` to `WithTx`: the Idempotency-Key middleware hooks into it (`db.WithCommitHook`).
- **Time:** always from `core.Clock` (`clock.Now()`), never `time.Now()` directly, so tests can fake it. Store it with `db.FormatTime(t)` (fixed-width UTC text that sorts lexicographically) and parse it with `db.ParseTime(s)`.
- **IDs:** `INTEGER PRIMARY KEY`. `contacts` and `messages` use AUTOINCREMENT, because signed public tokens carry their IDs and an erased contact's ID must never be handed out again. So do `segments`, `emails`, `broadcasts` and `sequence_steps` (migration 0002), because stored references outlive their rows: a broadcast's audience (`segment_id`), segment rules (`clicked.broadcast_id`), message rows (`email_id`, `step_id`). A reused ID would silently point at another row. In the API, IDs are integers. Forms also have a `public_id` (random base32, 10 characters), which appears in their public URLs.
- **Errors:** return `core.Err*` sentinel-wrapping errors (§4.1). The API layer maps them to HTTP. Wrap errors with `fmt.Errorf("…: %w", err)`.
- **Logging:** a `log/slog` JSON handler writes to stdout. Never log secrets, tokens or full email bodies. Log email addresses only at debug level. `server.RedactPath` removes tokens from logged paths.
- **Audit:** every write through the API or UI calls `audit.Log` in the same transaction. The CLI bootstrap commands are logged with actor type `cli`.
- **Tests:** they must pass with no network. Use `testutil.NewDB(t)` (a temp file DB with the migrations applied), `testutil.NewClock`, `testutil.NewFakeProvider()`. Tests are table-driven where it helps. `go test -race` needs cgo, so nothing relies on it. On Windows, run tests with `scripts/test.sh` (README).
- **Validation:** emails are checked with `core.NormalizeEmail` (trim, lowercase, `net/mail` parse, a single `@`, a domain with a dot, max 254). Tags are trimmed, 1–64 characters, and unique ignoring case.
- **Public endpoints** (`/f`, `/u`, `/c`, `/o`, `/hooks/ses`, `/media`, `/archive`) are plain `http.Handler`s mounted on `server.PublicMux` by `internal/app`, not Huma operations.

## 4. Shared contracts (`internal/core`, `internal/hooks`, `internal/outbox`, `internal/contactstore`)

### 4.1 Types and errors (`core`)

```go
type ContactStatus string // "pending","active","unsubscribed","bounced","complained","suppressed_manual"
const (StatusPending, StatusActive, StatusUnsubscribed, StatusBounced, StatusComplained, StatusSuppressedManual ContactStatus = ...)

type Contact struct {
    ID int64; Email, FirstName, LastName string
    Attributes map[string]any; Tags []string
    Status ContactStatus; StatusChangedAt, CreatedAt, UpdatedAt time.Time; Version int64
}

type MessageKind string   // "broadcast","sequence","optin","test","alert"
type MessageStatus string // "queued","sending","sent","delivered","bounced","complained","failed","skipped"
func IsMarketingKind(MessageKind) bool              // broadcast, sequence
func CanReceive(ContactStatus, MessageKind) bool    // the §6 send rules
const DataFromName, DataReplyTo = "from_name", "reply_to" // sender overrides in messages.data (test sends)

type Actor struct { Type string /* user|token|system|public|cli */; ID string; Name string }
func ActorFromContext(ctx) Actor; func WithActor(ctx, Actor) context.Context

var (
    ErrNotFound      = errors.New("not found")           // 404
    ErrConflict      = errors.New("conflict")            // 409
    ErrValidation    = errors.New("validation")          // 422 (use core.ValidationError / core.Invalid(field, ...))
    ErrPrecondition  = errors.New("precondition failed") // 412 (If-Match mismatch)
    ErrForbidden     = errors.New("forbidden")           // 403
    ErrSuppressed    = errors.New("address suppressed")  // 409 conflict (the message says why)
)
type ValidationError struct { Field, Message string } // Unwrap() == ErrValidation

type Clock interface { Now() time.Time }  // core.SystemClock{}
func NormalizeEmail(s string) (string, error)
func EmailHash(normalized string) string   // sha256 hex
```

### 4.2 Cross-package interfaces (`core`)

W1 and W2 packages implement these, and `internal/app` wires them, so those packages don't import each other.

```go
// Implemented by content. Used by sending (at send time) and by previews.
type Renderer interface {
    Render(ctx context.Context, in RenderInput) (Rendered, error)
}
type RenderInput struct {
    MessageID   int64          // 0 for previews: tracking/unsubscribe links become "#preview"
    EmailID     int64          // 0 when SystemTemplate is set
    SystemTemplate string      // "optin_confirm" | "resubscribe_confirm" | "alert"
    Contact     *Contact       // nil for alert/test-to-address; merge fields then use fallbacks
    ToEmail     string
    Data        map[string]any // messages.data; {"test": true} marks a test send (never live links)
    Marketing   bool           // true => wrap in layout with unsubscribe footer, rewrite links for click tracking
}
type Rendered struct {
    Subject, HTML, Text string
    UnsubscribeURL string       // "" when !Marketing
}

// Implemented by segments. Used by broadcasts and contacts (bulk tag).
type SegmentResolver interface {
    // WhereSQL returns a SQL boolean expression over alias "c" (contacts) plus its args.
    // It never includes the status filter; callers add "c.status = 'active'" when sending.
    WhereSQL(ctx context.Context, segmentID int64) (string, []any, error)
}

// Implemented by segments. Used by contacts inside the tag rename / merge / delete tx:
// saved segments refer to tags by name, so they follow renames and merges, and a tag
// a segment uses can't be deleted (409).
type TagReferrer interface {
    ReplaceTagName(ctx context.Context, tx *sql.Tx, from, to string) (int, error)
    SegmentsUsingTag(ctx context.Context, tx *sql.Tx, name string) ([]int64, error)
}

// Implemented by sequences. Used by forms (sign-up and opt-in confirmation).
type Enroller interface {
    Enroll(ctx context.Context, tx *sql.Tx, sequenceID, contactID int64, opts EnrollOptions) (enrollmentID int64, err error)
}
type EnrollOptions struct {
    Source  string     // "form:<public_id>" | "tag:<name>" | "api" | "admin" | "migration"
    AtStep  int        // 0/1 = first step; N = start at position N
    StartAt *time.Time // nil = now; next_run_at = StartAt + step.delay
}
// Enroll returns (0, nil) when it skips: the reentry policy, a missing or non-active contact,
// a contact that already has an exit tag, or a sequence without steps.

// Implemented by sending. The email provider abstraction (SES in production, fake in tests).
type EmailProvider interface {
    Send(ctx context.Context, m OutgoingEmail) (providerMessageID string, err error)
}
type OutgoingEmail struct { From, To, ReplyTo string; Raw []byte /* full RFC 5322 message */; Tags map[string]string }
type ProviderError struct { Retryable bool; Code, Message string } // returned (wrapped) by providers
```

Tag-triggered enrolment doesn't go through `Enroller`: it goes through the hooks (§4.3).

### 4.3 Hooks (`hooks`)

This is a synchronous in-process bus. Handlers run inside the publisher's transaction. A handler error rolls the publisher back, so handlers must be quick and must not do network I/O.

```go
type Bus struct{ ... }
func (b *Bus) OnTagAdded(fn func(ctx context.Context, tx *sql.Tx, e TagAdded) error)
func (b *Bus) OnStatusChanged(fn func(ctx context.Context, tx *sql.Tx, e StatusChanged) error)
func (b *Bus) OnContactCreated(fn func(ctx context.Context, tx *sql.Tx, e ContactCreated) error)
func (b *Bus) PublishTagAdded(ctx, tx, TagAdded) error   // etc.

type TagAdded struct { ContactID, TagID int64; TagName, Source string }
type StatusChanged struct { ContactID int64; From, To core.ContactStatus; Source string; MessageID int64 }
type ContactCreated struct { ContactID int64; Source string }
```

The only subscriber is `sequences` (`RegisterHooks`). On TagAdded it enrols contacts in active sequences whose trigger is that tag, and exits enrollments on an exit tag. On StatusChanged it exits active enrollments when the status is no longer active. `contactstore` publishes the events. `internal/app` builds one `Bus` and one `contactstore.Store` on it, and passes them to every package.

### 4.4 Outbox (`outbox`)

```go
type MessageSpec struct {
    Kind core.MessageKind
    BroadcastID, EnrollmentID, StepID, EmailID, ContactID int64 // 0 = NULL
    SystemTemplate string
    ToEmail string            // required; the contact's email for contact messages
    Data map[string]any
    NotBefore *time.Time      // nil = now
}
// Enqueue inserts one queued message. It returns (id, false, nil) if the row already exists
// (the UNIQUE (broadcast_id, contact_id) / (enrollment_id, step_id) constraint), so callers are idempotent.
func Enqueue(ctx context.Context, q db.Querier, clock core.Clock, s MessageSpec) (id int64, created bool, err error)
```

Suppression isn't checked at enqueue time. The sender checks it immediately before the provider call (D-6).

### 4.5 Contact store (`contactstore`)

Every package that needs contacts uses this. `contacts` [W1] builds the API, search and GDPR on top of it.

```go
type Store struct{ DB *db.DB; Bus *hooks.Bus; Clock core.Clock }
func (s *Store) Get(ctx, q db.Querier, id int64) (core.Contact, error)
func (s *Store) GetByEmail(ctx, q db.Querier, email string) (core.Contact, error)
func (s *Store) IsSuppressed(ctx, q db.Querier, normalizedEmail string) (bool, error)          // suppressions table
func (s *Store) Create(ctx, tx *sql.Tx, in CreateInput) (core.Contact, error)                  // ErrConflict if email exists, ErrSuppressed if in suppressions; publishes ContactCreated
func (s *Store) Update(ctx, tx, id int64, in UpdateInput, ifVersion int64) (core.Contact, error) // names/attributes; ifVersion 0 = skip check; bumps version
func (s *Store) AddTags(ctx, tx, contactID int64, names []string, source string) error         // creates tags as needed; publishes TagAdded only for newly added tags
func (s *Store) RemoveTags(ctx, tx, contactID int64, names []string) error
func (s *Store) SetStatus(ctx, tx, contactID int64, to core.ContactStatus, ch StatusChange) (changed bool, err error)
    // enforces allowed transitions (§6), writes contact_status_changes, publishes StatusChanged;
    // keeps the suppressions table in step: → suppressed_manual adds the manual hash row,
    // suppressed_manual → active removes it (Create with status suppressed_manual adds it too)
func (s *Store) RecordConsent(ctx, tx, e ConsentEvent) error
func (s *Store) RecordOptOut(ctx, tx, contactID int64) (recorded bool, err error)
    // an unsubscribe from a bounced/complained/suppressed_manual contact (the status can't change):
    // sets contacts.unsubscribe_requested_at; SetStatus → active from bounced or suppressed_manual
    // then lands on unsubscribed instead
type StatusChange struct { Source, Actor, Note string; MessageID int64 }
```

Each write (Create, Update, AddTags, RemoveTags, SetStatus) runs in a SAVEPOINT of the caller's tx. When it returns an error (validation, a refused transition, a hook handler error), nothing it did remains, including earlier hook handlers' writes. A bulk caller (import, bulk tag) can therefore record the row as failed and continue with the same tx.

### 4.6 Alerts (`core.Alerter`)

```go
// Implemented by sending. Used by sending, system, broadcasts and events.
type Alerter interface {
    Alert(ctx context.Context, tx *sql.Tx, key, title, body string) error // tx nil = own tx; dedup per key for 6 h
}
```

Alerts are `alert` messages to `settings.alert_email` (or `owner_email` if that's empty). They are never suppressed. `Alert` only enqueues. The keys raised as built:

| Key | Raised by | When |
|---|---|---|
| `circuit_breaker:<id>` | broadcasts | a broadcast's hard-bounce or complaint rate exceeded the threshold (B-6) |
| `broadcast_snapshot_failed:<id>` | broadcasts | the audience couldn't be resolved or queued; the broadcast is paused |
| `ses_sending_paused`, `ses_sending_resumed`, `ses_credentials_rejected` | sending | SES disabled or refused sending, enabled it again, or rejected the access key |
| `sns_unsubscribed` | events | SNS confirmed that the webhook subscription was removed |
| `backup_failed` | system | a backup (nightly or the `backup` job) failed, or its S3 upload failed |
| `cert_renewal_failed`, `backup_stale`, `disk_space_low` | system (health-watch, daily) | the HTTPS certificate check, the last-backup-under-26-h check or the disk check failed |

## 5. Configuration (`config`)

Environment variables are loaded once at start (`config.LoadFrom`). Every validation problem is reported at once, and the server refuses to start with any of them. Infrastructure values come **only** from the environment. Business settings are seeded from the environment on the first start, and after that the settings API owns them (100 % API).

| Env | Meaning | Seeds setting |
|---|---|---|
| `MAILROOM_DATA_DIR` | default `/data`: `mailroom.db`, `media/`, `backups/`, `imports/`, `secret.key`, `autocert/`. A relative path is made absolute | |
| `DOMAIN` | **required**: the one sending domain (read-only in the API) | |
| `APP_SUBDOMAIN` | default `newsletter` → base URL `https://newsletter.DOMAIN` | |
| `BASE_URL` | optional override, scheme and host only (e.g. `http://localhost:8080` in dev). `http` needs `HTTP_ONLY=1` | |
| `MAIL_FROM_SUBDOMAIN` | default `bounce` | |
| `FROM_NAME`, `FROM_EMAIL`, `REPLY_TO_EMAIL`, `COMPANY_FOOTER`, `OWNER_EMAIL`, `TIMEZONE` | `FROM_EMAIL`/`REPLY_TO_EMAIL` must be on `DOMAIN`. `TIMEZONE` is an IANA name, default `UTC`. Without `COMPANY_FOOTER` no marketing email goes out (E-1): broadcast send and sequence resume are refused, the sender holds broadcast and sequence messages (`FooterWait`, alert `company_footer_missing`) and doctor fails; the API refuses to clear it | yes |
| `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | the **sending** user's credentials (the two keys together or neither). Without all three, the sender and the SES account poller don't run | |
| `SES_CONFIGURATION_SET` | default `mailroom` | |
| `SNS_TOPIC_ARN` | the pinned topic (X-2). Its region also sets the SNS certificate host | |
| `BACKUP_BUCKET` | optional S3 bucket | |
| `MAILROOM_SECRET` | HMAC key, at least 32 characters. If it is empty, a key is generated once and stored in `<data>/secret.key` (0600) | |
| `HTTP_ONLY`, `HTTP_ADDR` | `HTTP_ONLY=1` (or `serve --http-only`) serves plain HTTP on `HTTP_ADDR` (default `:8080`) | |
| `TRUSTED_PROXIES` | comma-separated IPs/CIDRs of reverse proxies whose `X-Forwarded-For` is believed (e.g. `172.16.0.0/12`). Default none: the connection address is the client. `HTTP_ONLY` alone never trusts the header | |
| `MAILROOM_ADMIN_EMAIL`, `MAILROOM_ADMIN_PASSWORD`, `MAILROOM_BOOTSTRAP_TOKEN` | optional first-start bootstrap for platforms without a shell (Coolify): `serve` creates the first admin while no user exists, and an admin API token named `bootstrap` (the given `mr_…` secret, stored hashed) while no token of that name exists. Remove them after the setup | |
| `MAILROOM_LOG_LEVEL` | `debug`, `info` (default), `warn` or `error` | |

Read only by the CLI: `CLOUDFLARE_API_TOKEN` (`provision dns --cloudflare`), and the setup user's `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_SESSION_TOKEN` for `provision ses`, passed to that one run. `mailroom healthcheck` reads only `HTTP_ONLY` and `HTTP_ADDR`. The compose file uses `MAILROOM_VERSION` (build arg) and `MAILROOM_UID`/`MAILROOM_GID`.

## 6. Contact status rules (enforced in `contactstore.SetStatus`)

| From → To | Allowed? |
|---|---|
| pending → active | only with source `optin` (opt-in confirmed), or `admin`/`api` with a non-empty note |
| pending → any other | yes |
| active → unsubscribed / bounced / complained / suppressed_manual | yes |
| unsubscribed → active | only with source `optin` (a confirmed resubscribe), or `admin`/`api` with a non-empty note |
| bounced → active | only `admin`/`api` with a non-empty note |
| complained → active | **never** |
| suppressed_manual → active | only `admin`/`api` (removing a manual suppression) |
| unsubscribed / suppressed_manual → bounced, complained; bounced → complained | yes (recorded SES facts) |
| any other move between non-active statuses (e.g. bounced → unsubscribed, unsubscribed/bounced → suppressed_manual) | no: it would make reactivation easier. To block such an address, add it to the `suppressions` table; its status already stops sending |
| anything → pending | never |

API callers record their own source (`api`), and UI actions record `admin`, so U-4 attribution stays true. Refused transitions return `core.ErrForbidden` (403).

Send rules (`core.CanReceive` plus the sender's final check, D-6):
- Only `active` contacts receive `broadcast` and `sequence` messages.
- `optin` messages go to `pending` and `unsubscribed` contacts (for resubscribe) and to `active` ones (a double opt-in form submitted for an existing subscriber), never to bounced, complained or manually suppressed ones.
- `test` messages carry no contact. They are skipped when the address is in the suppressions table, or belongs to a contact that isn't `active` or `pending`, so a test never reaches an address that unsubscribed, bounced, complained or was suppressed.
- `alert` messages are never suppressed.
- Every kind except `alert` is also checked against the suppressions table. Sequence messages are skipped when their enrollment has exited.

An unsubscribe from a contact that is bounced, complained or suppressed_manual can't change its status (see the table), but it is never lost. `contactstore.RecordOptOut` sets `contacts.unsubscribe_requested_at`, and a later bounced/suppressed_manual → active (an admin lifting the bounce or the suppression) then lands on `unsubscribed`. This covers every way the request can arrive:
- **One-click or the page's button.** The events row is written too. The `GET /u/{token}` page shows such a contact the Unsubscribe button (without "Only stop these emails") until a request is recorded, so the link path reaches the POST that records it.
- **An admin or API caller** setting the status to `unsubscribed` (a request received by phone or email): the status stays, the marker is set, the status-change log gets a row with the same from and to status and the note `opt-out recorded`, and the call answers 200 with the unchanged status and `unsubscribe_requested_at` set.
- **An unsubscribed contact that later bounces or complains** (a late SES bounce, an import with `status=bounced`): `setStatus` keeps the opt-out by setting the marker to the time it unsubscribed.

"Only stop these emails" (exit reason `unsubscribed_sequence`) also stops a last step that the scheduler already queued (a completed enrollment becomes exited). That sequence then never enrols the contact again, whatever its re-entry policy.

A move to active from pending, unsubscribed or suppressed_manual through the API or UI is appended to the consent log (type `api`/`admin`, with the note as the text).

## 7. API conventions (`api`, used by every package)

- **Base path and registration.** The base path is `/api/v1`. Every package exposes `func (s *Service) RegisterAPI(a *api.API)`, which registers operations with `api.Register(a, op, handler)`. That is a thin wrapper around `huma.Register`. It also records the operation in the route registry (`a.Routes()`, used by the parity test and `TestOperationsRegisteredOnce`). It panics without `OperationID`, `Summary`, `Description` and `Tags`, and on a duplicate ID.
- **Extra success statuses.** `Metadata[api.MetaAlsoResponds] = map[int]string{200: "Dry run: ..."}` documents further 2xx statuses with the default response's body (dry runs of 202/201 operations, the 202 job answer of a large enrolment), so generated clients expect them. A create operation (default 201) whose input embeds `api.DryRunParam` gets this automatically: `api.Register` adds a middleware that answers a dry run with 200 and no `ETag`/`Location`, and documents the 200.
- **Scopes.** The default scope is `api.ScopeRead` for GET and `api.ScopeAdmin` for writes. `Metadata[api.MetaScope]` overrides it, e.g. read-only POST previews are `read`, and `download-backup` is `admin`. `Metadata[api.MetaSensitive] = true` marks a response that holds a secret shown once: `Cache-Control: no-store`, and it is never stored for replay.
- **Auth:** `Authorization: Bearer mr_<token>`, or the UI session cookie `mailroom_session` plus the `X-CSRF-Token` header for non-GET requests. The middleware puts a `core.Actor` in the context. `/api/v1/openapi.json`, `/api/v1/openapi.yaml` and `/api/v1/docs` need no token.
- **Errors:** `{"error": {"code": "not_found|conflict|validation|precondition_failed|forbidden|unauthorized|rate_limited|internal", "message": "...", "field": "..."}}`. The code set is closed. `api.MapError(err)` converts `core.Err*` (`ErrSuppressed` → 409 `conflict`). Huma's own errors go through `huma.NewError` (overridden) and `api.CodeForStatus`: 405 → `not_found`, other 4xx (malformed JSON, body too large, media type) → `validation`, 5xx → `internal`.
- **Pagination:** `?limit=` (default 50, max 500) and `?cursor=` (opaque base64 of the last ID). Responses are `{"items": [...], "next_cursor": "..."}`.
- **ETag / If-Match:** a single-resource GET returns `ETag: "v<version>"`. PATCH/PUT/DELETE accept `If-Match`, and a mismatch returns 412. Use `api.IfMatchParam` (embedded in the input) or `api.ParseIfMatch(header) (int64, error)`.
- **Idempotency-Key** on POST: `net/http` middleware that stores (actor, key) → response for 24 h. The `idempotency-cleanup` worker purges expired keys hourly.
  - The request hash covers the whole body. Huma closes the body when it has read it, and a real server's body can't be read after Close, so the middleware's body wrapper drains the unread rest into the hash on Close. The e2e harness uses bodies that fail after Close, like a server's.
  - The key is recorded inside the first write transaction the request commits (via `db.WithCommitHook`, so every service's `d.WithTx` with the request ctx joins in). The side effects and the key therefore commit together.
  - The same key with a different request hash returns 422, also after a failed attempt. A failed attempt that committed nothing may be retried with the same request.
  - When the request committed but its response isn't kept (`MetaSensitive` operations, responses over 1 MiB, a crash after the commit), a retry returns 409 `conflict`. A concurrent request with the same key also returns 409.
  - `api.PurgeStoredResponses(ctx, q, value)` drops stored responses that contain a value (GDPR erase). Their keys then answer 409, like an unkept response.
- **Dry run:** `?dry_run=true` where the PRD requires it. The handler runs the logic in a transaction and rolls back, returning what would happen.
- **Jobs:** long work returns `202 {"job": {...}}`. Clients poll `GET /api/v1/jobs/{id}`.
  - Submit with `runner.Submit(ctx, tx, type, params)` inside the operation's transaction, together with its `audit.Log`. The job exists and runs only if that tx commits, so a `?dry_run=true` rollback leaves no job.
  - `JobContext.SetProgress` never blocks and may be called inside the handler's own transaction.
  - Queued jobs survive a restart and run at the next start. Jobs running at a restart or shutdown fail with `"interrupted"`, unless their type is registered with `jobs.Resumable()`. Resumable jobs are queued again and re-run from the start (`JobContext.Resumed()` is true), so their handler must be idempotent.
  - Job types as built: `import.contacts`, `tags.bulk`, `sequences.enroll` and `broadcast.snapshot` (all resumable), and `backup`.
  - Work that must finish after a crash is either a resumable job, or re-derived by the owning package's worker from its own state (the broadcast-starter re-submits snapshots whose job failed).
- **Rate limit:** a token bucket per actor (default 20 req/s, burst 60), with `RateLimit-Limit/Remaining/Reset` headers and 429 when exceeded. Anonymous and public-endpoint limits are keyed by `api.IPKey(api.ClientIP(r, cfg.TrustedProxies))`: IPv4 addresses, IPv6 /64 networks, and `X-Forwarded-For` only from `TRUSTED_PROXIES`. Limiters keep at most 100 000 keys, and beyond that new keys share one bucket. The public limits are per client address:
  - form submissions (forms), with a separate, more generous limit for the hosted form pages;
  - click and open recording (events);
  - the web archive (`app`: 2/s, burst 30);
  - the media files (`app`: 20/s, burst 300, since a mail client loads all images at once);
  - the unsubscribe page, `GET /u/{token}` (events: 1/s, burst 60).

  Two public endpoints are deliberately not limited, a deviation from PRD §12 ("all public endpoints rate-limited"): the unsubscribe POST (one-click and the page's button), because mail providers send one-click unsubscribes for many people from a few shared addresses and none may be refused (U-3); and the SNS webhook, because SNS posts every SES event from a few AWS addresses and a 429 would drop bounces and complaints. HMAC tokens and SNS signatures are checked before either touches the database.
- **Login limits (A-4, `auth`):**
  - Each attempt is counted before the password is checked.
  - Per 15 min: 5 failures per email + client address, 20 per client address, and 20 per email. Addresses that logged in to the account in the last 30 days are exempt from the per-email budget.
  - 10 wrong authenticator codes per email per 24 h. TOTP codes can't be reused (`users.totp_last_step`). A missing code is reported like a wrong password.
  - Sessions slide for 14 days and end at most 30 days after the login. They also end when the password, email or TOTP changes.
- **Server:** read timeout 2 min and write timeout 10 min. `create-import` extends both to 30 min for its own upload. Every response carries baseline security headers. The admin UI sets its own stricter CSP, and HSTS is sent when `BASE_URL` is https.

## 8. Workers (`server.Worker`)

```go
type Worker interface { Name() string; Run(ctx context.Context) error } // returns when ctx is cancelled
```

`internal/app` collects them and `server.Supervisor` runs them. `TestWorkers` pins the set.

| Worker | Owner | Cadence |
|---|---|---|
| `sender` | sending | continuous: 4 senders share the token bucket. The dispatcher polls the queue every 1 s when idle. Every 5 s it also completes finished broadcasts (`CompleteBroadcasts`). At start it fails messages left in `sending` (`RecoverInterrupted`). **Not started without SES credentials** |
| `ses-account-poller` | sending | every 5 min (quota, sending enabled; feeds the bucket at `rate_fraction` × MaxSendRate). Not started without an SES client |
| `scheduler` | sequences | every 60 s (`RunDue`) |
| `broadcast-starter` | broadcasts | every 10 s (`Tick`): starts due scheduled broadcasts, re-submits failed snapshots, runs the circuit breaker |
| `sns-subscriber` | events | once at start, with retries (X-7). It needs `SNS_TOPIC_ARN` and an SNS client |
| `optin-expiry` | forms | hourly |
| `backup` | system | nightly at 02:30 in the settings time zone, retried after 15 min, 1 h and 3 h if it wrote no file |
| `retention` | system | daily, the first run 5 min after start |
| `health-watch` | system | daily, the first run 15 min after start. It runs the cert, backup-age and disk checks and alerts (§4.6) |
| `jobs` | jobs | continuous: claims queued jobs, polls every 1 s when idle |
| `idempotency-cleanup` | api | hourly |
| `ui-doctor` | ui | 30 s after start, then every 10 min. It caches the doctor report for the dashboard, so page loads never run network checks |

On shutdown, the HTTP server stops first (20 s). Then the workers get their context cancelled and at most 30 s to return (`server.Supervisor.RunUntil`, `app.WorkerDrainTimeout`). The process then exits anyway. The compose file's `stop_grace_period` is 60 s.

Without SES credentials, messages stay `queued` and broadcasts stay `sending`. When the credentials are added (`provision ses`, then `docker compose up -d`), the sender picks them up at the next start.

## 9. Test strategy

- Unit tests per package, against a real temp SQLite DB.
- `internal/app` has end-to-end tests (`e2e_*_test.go`, `flows_test.go`) through the real HTTP handler, with a `FakeProvider`, a fake clock and `app.Options{Offline: true}`, so nothing reaches the network. Workers are driven by their step functions (`sequences.RunDue`, `sending.SendDue`, `broadcasts.Tick`), never by sleeping. Only the jobs runner runs in the background, and tests wait for its jobs. SNS notifications are signed with a test certificate served by the injected webhook HTTP client. The tests cover:
  - bootstrap with the CLI's functions (`app.CreateAdmin`, `app.CreateToken`), then token and session (CSRF) access;
  - a layout, a single opt-in form and a three-step sequence built through the API: sign-up → drip with List-Unsubscribe headers → exit tag mid-way;
  - double opt-in: the contact stays pending until the confirmation POST (the GET page changes nothing);
  - a 200-contact segment broadcast with a restart mid-send → exactly one message per contact, broadcast sent; a crash with a message in `sending` → failed, never re-sent;
  - bounce and complaint through signed SNS (v1 and v2, redelivery, forgery, foreign topic) → suppression → no further sends, drip exited;
  - one-click unsubscribe (RFC 8058) → excluded from the next broadcast;
  - every suppression path (unsubscribed, bounced, complained, suppressed_manual, erased) against mail already queued (D-6) and the next broadcast; erased and manual addresses can't come back by form, import or API;
  - an unsubscribe while manually suppressed or bounced survives lifting the suppression or the bounce; "only stop these emails" after the last step was queued stops it and blocks re-entry; test sends skip opted-out addresses;
  - GDPR export, then erase (including test sends to the address and with its data): no table holds the address afterwards;
  - CSV import dry run (writes nothing, no job) then commit, with statuses and consent;
  - Idempotency-Key replay, ETag/If-Match 412, read-scope tokens can't write;
  - the circuit breaker pausing a broadcast on fake bounces, the owner alert, and a confirmed resume;
  - tag renames following into segments, and a broadcast's email being locked while it sends.
- Wiring tests in `internal/app`:
  - `TestOperationsRegisteredOnce`: every operation is in `expectedOperations`, in the spec, with a summary and description;
  - `TestSpecSnapshot`: `api/openapi.yaml` is current;
  - `TestWorkers`, `TestPublicRoutesMounted`, `TestAdminUIMounted`, `TestRunShutsDownInOrder`.
- `cmd/mailroom` checks the Dockerfile and `../deploy/docker-compose.yml` against the binary, since the tests run without Docker. It checks the entrypoint, the version stamp, `ARG GO_VERSION` ≥ the go.mod `go` line, the vendored build, the env-file mount and the health-check command. It also tests the CLI commands (bootstrap, migrate with backup-first, backup/restore, doctor, serve).
- **UI/API parity test** (`internal/ui/parity_test.go`, PRD G-1):
  - every UI action is registered with `u.handle(Action{Method, Path, OperationID, Uses, UIOnly, Handler})` and names an API operation that exists in `api.Routes()`;
  - a GET action never claims a write operation;
  - every API write has a UI action, or an entry in `parityAllowlist` with a reason (e.g. `upsert-contact`: integration-only);
  - with `strictParity` set to false, a missing UI action is only logged.
- UI handler tests use httptest through the real app. Each page renders for a signed-in user, redirects to the login page when logged out, and rejects a POST without CSRF.

## 10. Decisions and deviations (as built)

These are the decisions the build made where the PRD or the first version of this file said something else, or said nothing. The PRD's "Build notes" list the ones that change a requirement.

**Content and rendering**
- **Merge fields use an own engine**, not `html/template` (E-3). `content/merge.go` parses `{{first_name}}`, `{{last_name}}`, `{{email}}`, `{{attr.<key>}}` and `{{unsubscribe_url}}`, each with an optional `| "fallback"`. Unknown fields are rejected when an email is saved, including GHL's `{{contact.*}}`, with a hint. Values are HTML-escaped in HTML and inserted as-is in the subject and the text part. In URL attributes, only http/https/mailto/tel values may start a URL. Links whose href contains a merge field are not click-tracked. The admin UI uses `html/template`.
- **Test sends** (B-7, S-10) are `test` messages with `{"test": true}` in `messages.data`, and optionally `contact_id` for a real contact's merge values plus the broadcast's sender overrides. They always get the inert `#preview` unsubscribe link and no tracking. They don't count in stats. Like every kind, they are checked at send time (§6).
- **Broadcast email lock:** a broadcast owns its email. `content.UpdateEmail` refuses edits (409) once the broadcast is past `scheduled`, also through `/emails/{id}`. A broadcast's ETag version is `broadcasts.version + emails.version - 1`.

**Sending**
- **Delivery is at most once.** A message is sent only after its `queued → sending` switch has committed. Rows found in `sending` at start are failed as interrupted (`RecoverInterrupted`). Provider calls that may have reached SES without a response fail with code `delivery_unknown`. Only errors that prove the request never left, or retryable error responses (throttling, 5xx), are retried: up to 5 tries with backoff (D-4).
- **The sender completes broadcasts** (B-2). `sending.CompleteBroadcasts` sets `sent` once `snapshot_done_at` is set and no message of the broadcast is queued or sending. The broadcasts package never sets `sent`. This is the one place where sending writes the `broadcasts` table.
- **Global sending state.** The settings key `sending_state` pauses everything (SES paused or disabled, rejected credentials, or an operator via `POST /sending/pause`). It is lifted automatically when SES reports sending enabled again, and manually with `/sending/resume`.
- **Package state lives in settings.** The SES account snapshot is stored under the settings key `ses_account`, and the circuit-breaker baseline after a confirmed resume under `broadcast_breaker:<id>`, both through `settings.PutRaw`. There are no extra columns.

**Events and bounces**
- **Soft bounces.** Only a Permanent bounce sets `messages.status = 'bounced'`, so "bounced" in stats means a hard bounce. Transient and Undetermined bounces are `events` rows only, and they still count toward the 3-in-30-days rule (X-4). Complaints with feedback type `not-spam` are recorded but change nothing.
- **Deferred events.** An SES event for a message not recorded yet, and less than 2 minutes old, is answered 503 once per SNS MessageId (in memory), so SNS redelivers after the sender commits.
- **Raw event retention** (X-5): `events.detail` is emptied after 90 days, but the row stays, so stats and timelines keep working. Raw SES JSON is kept only on a known message's own contact row. Other rows hold an address-free summary. The raw JSON is stored without `mail.headers` and `mail.commonHeaders` (migration 0003 removed them from older rows): SES copies the original headers into every event, and `List-Unsubscribe` carries the recipient's working one-click URL, which a read-scope token must not be able to collect.
- **Stale tokens.** A token whose message no longer belongs to its contact (erased) acts on nobody.

**Other domains**
- **Restore** (L-6): `mailroom restore` pauses all sending in the restored database (a manual `sending_state` pause, reason "restored from a backup …"), so nothing goes out until an operator resumes. It then carries over from the replaced database's safety copy (`system.reconcile`, one transaction): the AUTOINCREMENT counters, suppressions, erasures (contacts erased there are erased again with `contacts.EraseRows`), blocking statuses and unsubscribe requests (only where §6 allows the move), opted-out contacts created after the backup, the state of messages queued in the backup and broadcast/sequence messages sent after it, how far active enrollments got, and per-sequence opt-outs (`unsubscribed_sequence`) on enrollments in any status or created after the backup. Credentials only get revoked: tokens revoked or missing there are revoked, users missing there (matched by ID and email) are deleted, users keep that database's password hash and TOTP settings, and restore deletes every session. Nothing is carried the other way (resubscribes, lifted suppressions, new content, new users or tokens). Without a replaced database, or when carrying over fails, the result's `warnings` say what is lost; the restore itself stands.
- **Retention** (O-7) is limited to event details (90 days), finished jobs (30 days) and expired sessions. Message rows are never aggregated or deleted, and neither are consent logs, status changes or suppressions.
- **Imports:** a dry run answers 200 `{"report": …}` directly, with no job. A commit answers 202 with a resumable job whose checkpoint is in `jobs.result`. Statuses only ever move away from active. Erased and manually suppressed addresses are rejected.
- **Forms:**
  - public URLs use `public_id` (`/f/{public_id}`, `/f/confirm/{token}`). The confirmation GET page changes nothing; the POST confirms;
  - a confirmation applies the form's *current* tags and sequence;
  - an existing active contact: a double opt-in form sends it a confirmation like a new address, and changes nothing before the click (F-6). A single opt-in form applies tags, consent and the sequence at once. Either way the public form (and a confirmation for an already active contact) only fills in names and attributes the contact doesn't have: it needs no login, so it must not rewrite what real mail merges in (`{{first_name}}`);
  - an open pending opt-in (its payload, IP, user agent and consent text) is replaced only together with a new confirmation email, so the link the contact already holds keeps applying what was submitted with it;
  - confirmation emails are limited to 1 per 10 min and 3 per 24 h per contact, and the answer is identical for every address state (F-7);
  - there is no disposable-domain list (optional in F-5).
- **Sequences:**
  - extra operations `pause-sequence` and `resume-sequence`;
  - reorder takes `on_active=keep|end`. Deleting a step with waiting enrollments needs `on_active=advance|exit`, and gives 409 without it;
  - changing a step's delay moves the due time of waiting enrollments;
  - a sequence without its own send window uses `settings.default_send_window` at send time;
  - tag triggers enrol only into active sequences.
- **Broadcasts:**
  - DELETE refuses a cancelled broadcast that has delivery history;
  - a resume after a circuit-breaker trip needs `confirm: true` (422 `confirm` otherwise);
  - `paused_reason` can be `snapshot_failed`;
  - the web archive (B-10) is `/archive/{id}`, only for audience-all broadcasts that are sending, paused or sent. It is noindex, with no contact data or tracking, and addressed by the sequential ID.
- **Content section of the UI.** `/admin/emails` (the email library: list, edit with If-Match, test send, delete after a dry run, with what uses each email from `get-email-usage`), `/admin/media` (upload, copy the public URL, delete) and the base layout share one nav item with section tabs. Steps and broadcasts keep their own editors; the library is where unused emails can be found and deleted.
- **Contacts by email.** `list-contacts` takes `email=` (exact, normalised): the UI's preview and test forms accept a contact's email address (or ID) and resolve it with it.
- **DKIM target zone.** `core.SESDKIMDomain` (the identity's SigningHostedZone, else the region's default) is used both by `provision` for the records it prints or creates and by the doctor's `dns_dkim` check.
- **DMARC** (`--dmarc`): `v=DMARC1; p=none`, with `rua=mailto:OWNER_EMAIL` only when that address is on the domain (reports to another domain need an authorization record there).
- **Dashboard** (A-3): `internal/stats` serves the numbers at `GET /api/v1/stats/dashboard`, so the UI shows nothing the API can't. The dashboard highlights a 30-day hard-bounce rate from 2 %. The numbers are cached for a minute, but only while a cheap stamp of the contacts (count, active count, latest status change) is unchanged, so an import, erase or status change shows at once.

**Not built**
- A separate "bounce rate over 2 % over 7 days" alert email (O-5).
- Outbound webhooks (G-10, P2).
- An admin IP allowlist (§12 security): use the firewall or a proxy.
- CI: the kit has none, so the tests and `govulncheck` run by hand (README).

**Provisioning**
- `provision ses` also creates the optional S3 backup bucket (SSE AES256, versioning, 30-day expiry of `backups/` and of overwritten versions; skip it with `--no-backup-bucket`), and can rotate the sending user's key (`--rotate-keys`).
- The sending user's `sns:Subscribe` is limited to Mailroom's own webhook (`StringEquals` on `sns:Protocol` and `sns:Endpoint`, from `BASE_URL` or `https://APP_SUBDOMAIN.DOMAIN`), because the topic's events carry every recipient address and a subscription outlives a rotated key. The doctor's `sns_subscription` check warns about any other subscription. A setup user without `s3:PutBucketVersioning` (an older guide) gets a warning instead of a failure.
- It writes to `--env-file` (default `/opt/mailroom/.env`).
- `provision dns` never changes or deletes records, and exits 2 on a conflict.

**Footprint:** the static binary is about 43 MB, mostly the AWS SDK. The image is about the binary plus distroless/static (a few MB).
