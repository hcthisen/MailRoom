# PRD: Mailroom, a single-app email tool to replace GoHighLevel email

_Mailroom is a working name._

| | |
|---|---|
| **Status** | Draft v0.3 |
| **Owner** | {{AGENCY}} |
| **Date** | 2026-09-29 |
| **Replaces** | listmonk + n8n in the GHL exit kit |
| **Nature** | One-off transition software, handed over as-is. Not a maintained product or SaaS |
| **Licence** | MIT (open source) |

---

## 1. Summary

Clients leaving GoHighLevel (GHL) use it for two email jobs:
1. **Drip sequences** triggered by sign-ups, e.g. "joined the free community → 5 emails over 7 days".
2. **Broadcasts** to the whole list or a segment, a handful of times a year.

The current kit plan uses two platforms for this. listmonk handles contacts and broadcasts, and n8n handles triggers and drips. n8n is a general automation platform, far more than a signup webhook and a timer need. It is also a second system to deploy, secure, back up and explain.

**Mailroom** is one small app that does exactly these jobs and nothing else:
- Go, one binary, one Docker container.
- One SQLite file.
- Sends through Amazon SES.
- **100 % API-enabled:** everything can be read and changed through a REST API, so an AI agent can operate and update the whole app.
- Built to be installed and operated by a coding agent (Claude Code or Codex) from the kit folder, with a simple web UI for the client.

Mailroom is **one-off software**. {{AGENCY}} builds it once so clients have a viable option when GHL closes, and hands it over as-is with its source code under the MIT licence. {{AGENCY}} does not maintain, update or support it afterwards. Each client owns their install, and can use their own coding agent to change or update it.

## 2. Goals

1. **One platform.** One container, one database file, one admin UI, one backup.
2. **Covers what clients use GHL email for:** contacts, tags, sign-up forms, linear drip sequences, broadcasts, unsubscribe, bounce handling, basic stats.
3. **Safe by default.** Suppressed contacts can never be emailed by any path. Every marketing email has one-click unsubscribe. A bad list can't burn the client's SES account (circuit breaker).
4. **100 % API-enabled.** Every record, setting and action in the app can be read and changed through the REST API. Nothing is UI-only. An AI agent can run and change the whole app without opening the UI.
5. **Small and cheap.** Runs comfortably on the smallest Hetzner VPS. Cost is the VPS plus SES at about $0.10 per 1,000 emails.
6. **Easy to load migrated data:** a documented CSV import format that carries consent data and unsubscribed/bounced status, and a YAML format for sequences.
7. **Self-contained handover.** The source, pinned dependencies and build instructions are all in the kit. Nothing depends on {{AGENCY}} infrastructure (no registry, update feed or licence server) after handover.

## 3. Non-goals

- SMS, calendars, pipelines/CRM, phone, landing-page or funnel builder.
- A drag-and-drop email builder. Content is written in Markdown or HTML in one base layout.
- Branching automations. Sequences are linear, with delays and stop conditions only.
- Multi-tenant or multiple sending domains. One install is one client with **exactly one sending domain**.
- A/B testing, dedicated IPs, per-contact send-time optimisation.
- Sending through anything other than SES in v1. A provider interface is kept so SMTP can be added later.
- Transactional app mail such as password resets and receipts. A client can send it through SES directly.
- Exporting from GHL. That is a separate step in the kit, independent of Mailroom (see §11).
- Any language other than English in the UI. No translation layer.
- Ongoing maintenance, a release channel or an update service.

## 4. Users

| User | Needs |
|---|---|
| **Client owner** (non-technical) | Send a newsletter, see who signed up, change the text or delay of a drip email, export or delete a contact on request |
| **Client's coding agent** | Install and provision via a few CLI commands, then configure, import, verify and change everything through the REST API. Needs a complete OpenAPI spec, predictable errors, dry runs and a `doctor` check |
| **{{AGENCY}} (builder)** | Build it once, test it on its own account, and hand it over in the kit. No obligations after that |

**Scale target per install:** up to 50,000 contacts, 200,000 emails/month, 10 sequences, 5 forms.

## 5. Design principles

1. **Suppression is global and checked at send time,** for every message type. listmonk's transactional API skips the blocklist; Mailroom has no such path.
2. **One row per message.** Every email gets a `messages` row *before* it is sent. That row gives exact per-recipient status, prevents double sends after a crash, and maps SES bounce and complaint events back via the SES MessageId.
3. **State lives in the database, not in timers.** Drip progress is an `enrollments` row with `next_run_at`. A restart or deploy never loses or duplicates a step.
4. **API-first.** The API and the admin UI call the same service layer, and every UI action has a matching API operation (checked by a test). The CLI only covers what can't go through the API: bootstrap, provisioning outside the app, and offline backup/restore.
5. **Few dependencies.** Standard library first. No external services beyond SES and SNS (and S3 for backups).

---

## 6. Functional requirements

Priority: **P0** = MVP (needed to shut down GHL), **P1** = soon after, **P2** = later.

### 6.1 Contacts and consent

| ID | Requirement | Prio |
|---|---|---|
| C-1 | A contact has: email (unique, compared lowercased), first name, last name, `attributes` (JSON), tags, status, created/updated time | P0 |
| C-2 | Status is one of `active`, `unsubscribed`, `bounced`, `complained`, `suppressed_manual`. Only `active` contacts receive marketing email _(plus `pending`: see Build notes)_ | P0 |
| C-3 | Every consent event is logged permanently: timestamp, source (form ID / import / API / admin), IP, user agent, the consent text shown, and double-opt-in confirmation time. The log is append-only | P0 |
| C-4 | Search and filter contacts by email, name, tag, status and created date. Paginated list in the UI | P0 |
| C-5 | GDPR export: one contact's data, consent log and message history as JSON (UI and API) | P0 |
| C-6 | GDPR erase: delete the contact and all personal data (tags, consent log, status history, enrollments), anonymise their messages and clicks (keep counts, drop the address and raw event data), and keep only a SHA-256 hash of the normalised email in `suppressions`, so the address can't be re-added by import or form | P0 |
| C-7 | Contact timeline in the UI: consent, tags, emails sent, bounces, clicks, unsubscribes | P1 |

### 6.2 Tags and segments

| ID | Requirement | Prio |
|---|---|---|
| T-1 | Tags are free-form labels on contacts. Add or remove them from the UI, API, CSV import or form | P0 |
| T-2 | A segment is a saved filter: tags (all/any/none), status, created date range, attribute equals/contains, and engagement (clicked any link, or a given link, in a given broadcast or within the last N days). Segments are evaluated when used, never stored as membership | P0 |
| T-3 | The segment builder shows a live count | P0 |
| T-4 | Adding a tag can trigger a sequence (see S-2) | P0 |

> Mailroom has no separate "lists". listmonk's lists + per-list subscription status become tags + one global status. That covers GHL usage (which is tag-based) and removes a whole class of "unsubscribed from list A but not B" confusion.

### 6.3 Forms and sign-up API

| ID | Requirement | Prio |
|---|---|---|
| F-1 | A form has: name, fields (email required; first name and custom fields optional), tags to apply, sequence to start, consent text, double opt-in on/off, success redirect URL | P0 |
| F-2 | Public endpoint `POST /f/{form_id}` accepting form-encoded or JSON data, with CORS for the client's domain | P0 |
| F-3 | Hosted form page `GET /f/{form_id}` so a client without a website developer can just link to it | P0 |
| F-4 | Embed snippet generator: copy-paste HTML for the client's website, shown in the UI and returned by `GET /api/v1/forms/{id}/snippet` | P0 |
| F-5 | Spam protection: honeypot field, per-IP rate limit, email syntax check, and optionally a disposable-domain list | P0 |
| F-6 | Double opt-in, **off by default** and switched on per form. When on, it sends a confirmation email and applies tags and the sequence only after the click. Unconfirmed sign-ups expire after 7 days | P0 |
| F-7 | Re-subscribe: a sign-up from an unsubscribed contact reactivates it only through a confirmation email, whatever the form setting. This is the only case where confirmation is forced. Bounced, complained and erased addresses are rejected silently (the response looks the same as success) | P0 |
| F-8 | Integrations use the same API (§6.12). Example: a payment system calls `POST /api/v1/contacts/{id}/tags` to add `customer`, so the contact exits a sequence | P0 |

### 6.4 Sequences (drips)

| ID | Requirement | Prio |
|---|---|---|
| S-1 | A sequence is an ordered list of steps. Each step has an email and a delay after the previous step (minutes/hours/days) | P0 |
| S-2 | Triggers: form sign-up (F-1), tag added (T-4), API enrol, manual enrol from the UI | P0 |
| S-3 | Exit conditions, checked **before every step**: contact not `active`, contact has any of the sequence's exit tags, contact unenrolled manually. On exit, optionally add a tag | P0 |
| S-4 | Re-entry policy per sequence: `never` (default), `after_completion`, or `always` | P0 |
| S-5 | Optional send window, e.g. Mon–Fri 08:00–20:00 in the install's timezone. A step that falls due outside the window waits for the next opening | P1 |
| S-6 | A scheduler runs every 60 s and processes due enrollments in batches. Each step creates a `messages` row inside the same transaction that advances the enrollment, so each step is sent exactly once | P0 |
| S-7 | Editing a sequence: editing a step's content applies to future sends. Adding steps at the end applies to active enrollments. Deleting or reordering steps asks what to do with active enrollments: keep their position by step ID, or move them to the end | P0 |
| S-8 | Enrol at a given step with a given start time, used when migrating contacts who are mid-drip in GHL | P0 |
| S-9 | Stats per step: sent, delivered, bounced, clicked, unsubscribed. Stats per sequence: active, completed, exited | P0 |
| S-10 | "Send test": send any step to the owner's address with example data | P0 |

### 6.5 Broadcasts

| ID | Requirement | Prio |
|---|---|---|
| B-1 | A broadcast has: name, subject, preheader, from (defaults from settings), content, audience (segment, or everyone active), and optional scheduled time | P0 |
| B-2 | Status: `draft → scheduled → sending → paused → sent`, plus `cancelled` from draft, scheduled or paused. Only the sender sets `sent` | P0 |
| B-3 | When sending starts, the audience is **snapshotted into `messages` rows** (status `queued`). Contacts who become non-active before their message goes out are skipped at send time | P0 |
| B-4 | Throttling: one global token bucket shared by broadcasts and sequences, set to 80 % of SES `MaxSendRate` (read from the SES API, overridable) | P0 |
| B-5 | Pause, resume and cancel from the UI and the API. After a crash or restart, sending resumes with the remaining `queued` messages | P0 |
| B-6 | **Circuit breaker:** automatic pause and an alert email to the owner if the hard-bounce rate goes above 5 % or the complaint rate above 0.1 % after the first 200 messages (thresholds configurable) | P0 |
| B-7 | Send a test to up to 5 addresses. Show a preview with a real contact's data | P0 |
| B-8 | Stats: recipients, sent, delivered, bounced, complained, unsubscribed, unique clicks, clicks per link | P0 |
| B-9 | Duplicate a broadcast | P1 |
| B-10 | Web archive page per broadcast (public URL, no personal data) | P2 |

### 6.6 Content and templates

| ID | Requirement | Prio |
|---|---|---|
| E-1 | One **base layout** per install: logo, brand colour, footer text (company name, address, registration no.), unsubscribe link. Every marketing email is wrapped in it | P0 |
| E-2 | Emails are written in Markdown (default) or raw HTML, with a live preview in the UI | P0 |
| E-3 | Merge fields: `{{first_name}}`, `{{last_name}}`, `{{email}}`, `{{attr.<key>}}`, each with a fallback, e.g. `{{first_name \| "there"}}`. Rendering uses Go `html/template` with a small fixed function set, no Sprig _(changed: see Build notes)_ | P0 |
| E-4 | The unsubscribe link is **always** in the footer, and saving is blocked if a custom HTML layout removes it | P0 |
| E-5 | A plain-text version is generated automatically from the HTML | P0 |
| E-6 | Images can be uploaded and served from the app (`/media/…`), with size limits. A remote URL can also be used | P1 |
| E-7 | Import raw HTML (e.g. copied from GHL) as the content of an email | P0 |

### 6.7 Sending (Amazon SES)

| ID | Requirement | Prio |
|---|---|---|
| D-1 | Send through the **SES v2 API** (`SendEmail` with raw MIME), not SMTP. This returns the SES MessageId, which is stored on the message row, and allows configuration-set and message tags | P0 |
| D-2 | Headers on every marketing email: `List-Unsubscribe: <https://…/u/{token}>, <mailto:…>` (optional), `List-Unsubscribe-Post: List-Unsubscribe=One-Click`, `Feedback-ID` | P0 |
| D-3 | Every send goes through the configuration set `mailroom` (event destination: SNS) | P0 |
| D-4 | Errors: throttling and 5xx responses are retried with exponential backoff (max 5 tries), then the message is `failed`. Permanent errors (e.g. `MessageRejected`) mark it failed right away. If SES sending is paused for the account, all sending is paused and the owner is alerted | P0 |
| D-5 | A provider interface (`Send(ctx, msg) (providerID, error)`) so an SMTP provider can be added later | P1 |
| D-6 | A final suppression check runs **immediately before** the API call, not only when the message is queued | P0 |
| D-7 | **Exactly one sending domain per install**, set at provisioning. All from and reply-to addresses must be on that domain (validated). The domain is read-only in settings, and changing it means re-provisioning | P0 |

### 6.8 Bounces, complaints and delivery events

| ID | Requirement | Prio |
|---|---|---|
| X-1 | Public endpoint `POST /hooks/ses` receives SNS notifications | P0 |
| X-2 | Verify the SNS signature (SignatureVersion **1 and 2**). The cert URL must be under `sns.<region>.amazonaws.com`, and the `TopicArn` must equal the configured ARN. Auto-confirm `SubscriptionConfirmation` only for that ARN | P0 |
| X-3 | Map events to messages through the SES MessageId. Handle **every** recipient in `bouncedRecipients` and `complainedRecipients`, not just the first | P0 |
| X-4 | Rules: `Permanent` bounce → contact `bounced`. `Transient` bounce → counted, and the contact becomes `bounced` after 3 transient bounces within 30 days. `Complaint` → contact `complained`. `Delivery` → message `delivered` | P0 |
| X-5 | Store raw events for 90 days for debugging, then delete them _(changed: see Build notes)_ | P0 |
| X-6 | Handling is idempotent: duplicate SNS deliveries don't double count | P0 |
| X-7 | On startup, if its own `/hooks/ses` URL isn't subscribed to the SNS topic, Mailroom subscribes it (the sending user may `sns:Subscribe` on that one topic only), so no second provisioning step is needed | P0 |

### 6.9 Unsubscribe

| ID | Requirement | Prio |
|---|---|---|
| U-1 | The unsubscribe token is an HMAC-signed `(contact_id, message_id)`. A token for one contact never works for another, and it doesn't unlock data export | P0 |
| U-2 | `GET /u/{token}` shows a confirmation page with one "Unsubscribe" button, and optionally "Unsubscribe from this sequence only" | P0 |
| U-3 | `POST /u/{token}` handles one-click unsubscribe (RFC 8058): no login, no confirmation, responds 200, and sets the contact `unsubscribed` right away. It stops all sequences and excludes the contact from future broadcasts | P0 |
| U-4 | Every unsubscribe records the time, the source (link / one-click / admin / API) and the message ID | P0 |

### 6.10 Tracking

| ID | Requirement | Prio |
|---|---|---|
| K-1 | Click tracking **per contact**: links are rewritten to `/c/{token}` and redirect with 302. Every click is stored with contact, message and link, so the client can build "clicked link X" segments. It can be switched off entirely, but there is no aggregate-only mode | P0 |
| K-2 | Open tracking: pixel `/o/{token}`. **Off by default** (Apple Mail privacy makes it unreliable) | P1 |
| K-3 | No tracking of links in unsubscribe or confirmation pages | P0 |
| K-4 | The default consent text on forms mentions that email engagement (clicks) is tracked per person | P0 |

### 6.11 Admin UI

| ID | Requirement | Prio |
|---|---|---|
| A-1 | Server-rendered HTML with htmx and no build step. Works on a laptop and is usable on a phone. **English only** | P0 |
| A-2 | Pages: Dashboard, Contacts, Segments, Forms, Sequences, Broadcasts, Settings, Health, API tokens | P0 |
| A-3 | The dashboard shows new contacts (7/30 days), unsubscribes, emails sent this month, bounce and complaint rate, SES quota, and warnings from `doctor` | P0 |
| A-4 | Users: owner plus optional extra users (same permissions in v1). Passwords hashed with argon2id, optional TOTP, secure session cookies, CSRF protection, login rate limit | P0 |

### 6.12 REST API (100 % coverage)

| ID | Requirement | Prio |
|---|---|---|
| G-1 | **Everything is in the API.** Every record, setting and action (all of §6.1–6.11 and §6.13) is available under `/api/v1/`. A test lists every UI action and fails if one has no matching API operation | P0 |
| G-2 | **The OpenAPI 3.1 spec is generated from the code** (Huma), so it can't drift, and request validation comes from the same definitions. It is served at `/api/v1/openapi.json`, a snapshot is kept in `app/api/openapi.yaml`, and every operation has a description and an example | P0 |
| G-3 | **An agent guide** at `app/docs/api.md` (also served at `/api/v1/docs`) with copy-paste `curl` recipes for common tasks: add a contact, tag a segment, change a drip delay, write and send a broadcast, read stats, export or erase a contact | P0 |
| G-4 | **Auth:** `Authorization: Bearer <token>`. Tokens have a name, a scope (`admin` = everything, `read` = GET only), an optional expiry and a last-used time. They are stored hashed, shown once, revocable, and managed through the API itself | P0 |
| G-5 | **Conventions:** JSON in and out. Cursor pagination (`?limit=&cursor=`). Filters as query params. `PATCH` for partial updates. Times in RFC 3339 UTC. One error format: `{"error": {"code", "message", "field"}}`. 422 for validation errors, 409 for conflicts | P0 |
| G-6 | **Safe writes for agents:** an `Idempotency-Key` header on every `POST` (retries don't create duplicates), `ETag` + `If-Match` on updates (an agent can't overwrite a change made in the UI, and gets 412 instead), and `?dry_run=true` on imports, bulk operations, erase and broadcast send (returns what would happen, e.g. the recipient count) | P0 |
| G-7 | **Long-running work** (imports, broadcast snapshot, backup, bulk tagging) returns `202` with a job resource: `GET /api/v1/jobs/{id}` gives status, progress and result | P0 |
| G-8 | **Audit log:** every write through the API or UI is logged with actor (user or token name), time, operation and the changed fields. Readable via `GET /api/v1/audit` | P0 |
| G-9 | Rate limit per token, with `RateLimit-*` response headers | P0 |
| G-10 | Outbound webhooks (e.g. `contact.created`, `contact.unsubscribed`, `message.bounced`) to URLs the client configures | P2 |

**API coverage** (all under `/api/v1`)

| Area | Operations |
|---|---|
| Contacts | list/search/filter, get, create, upsert by email, update, change status (with the suppression rules), add/remove tags, timeline, enrollments, GDPR export, GDPR erase |
| Tags | list, create, rename, merge, delete, bulk add/remove by segment or contact IDs |
| Segments | CRUD, count, preview members |
| Forms | CRUD, embed snippet, recent submissions |
| Sequences | CRUD, pause/resume, steps CRUD and reorder, enrol/unenrol (bulk, at a given step and start time), import/export as YAML (I-3), stats |
| Emails and layout | CRUD, render preview for a given contact, send test, get/update the base layout |
| Broadcasts | CRUD, duplicate, send test, dry-run count, schedule, send now, pause, resume, cancel, stats, recipients |
| Messages, events, clicks | list/filter, get |
| Suppressions | list, add, remove (manual suppressions only; bounces and complaints can't be removed) |
| Imports | upload CSV (I-1) with dry run or commit, job status, report |
| Media | upload, list, delete |
| Settings | get/update: sender name and addresses, footer, tracking on/off, send rate, circuit-breaker thresholds, timezone, default send window, alert address. The sending domain is read-only (D-7) |
| Users and tokens | CRUD users, reset TOTP, CRUD API tokens |
| System | health, version, `doctor`, SES account status and quota, backups (list, create, download), jobs, audit log |

Public endpoints outside the API (no token): `/f/*` forms, `/u/*` unsubscribe, `/c/*` click redirects, `/o/*` open pixel, `/hooks/ses`, `/healthz`.

### 6.13 CLI (bootstrap and provisioning only)

The CLI covers only what can't go through the API: work outside the app (AWS, DNS) and work before the app runs or while it's stopped. Every command supports `--json`. Exit codes: 0 ok, 1 error, 2 check failed.

| ID | Requirement | Prio |
|---|---|---|
| L-1 | `mailroom provision ses`: run once with the setup IAM keys passed as environment variables for that run only. It creates the SES identity (Easy DKIM), custom MAIL FROM, configuration set with SNS event destination, SNS topic, and the sending IAM user `mailroom-sender` with a least-privilege policy. It writes the sending user's keys to the server `.env` and prints the DNS records needed. Safe to run again | P0 |
| L-2 | `mailroom provision ses --request-production --use-case <file>` submits the SES production access request with the reviewed text | P0 |
| L-3 | `mailroom provision dns --cloudflare`: creates the needed records through the Cloudflare API. It **never modifies or deletes** existing records and prints a diff first (`--yes` to apply). `mailroom dns records` prints the same records as a copy-paste table for manual DNS | P0 |
| L-4 | `mailroom doctor` (same checks as `GET /api/v1/system/doctor`): DNS (A, DKIM, MAIL FROM MX/SPF, DMARC present), SES identity verified, production access, sending enabled and quota, SNS subscription confirmed, HTTPS certificate valid, last backup under 26 h old, clock skew, disk space. Every check has a status and a fix hint | P0 |
| L-5 | Bootstrap: `mailroom admin create` (first UI user) and `mailroom token create --scope admin` (first API token). After that, users and tokens are managed through the API | P0 |
| L-6 | Offline: `mailroom backup`, `mailroom restore <file>`, `mailroom migrate`, `mailroom version` | P0 |

### 6.14 Operations

| ID | Requirement | Prio |
|---|---|---|
| O-1 | **One container.** Serves HTTPS itself with automatic Let's Encrypt certificates (`autocert`) on 80/443. Can also run behind a reverse proxy (`--http-only`) | P0 |
| O-2 | Data in one volume: `mailroom.db` (SQLite in WAL mode) + `media/` | P0 |
| O-3 | Nightly backup via `VACUUM INTO`, kept locally for 14 days. Optional upload to an S3 bucket in the same AWS account (server-side encrypted, 30-day lifecycle). `mailroom restore` is tested in CI | P0 |
| O-4 | Health endpoint `/healthz`. Structured JSON logs to stdout | P0 |
| O-5 | Alert emails to the owner (sent via SES, never suppressed): circuit breaker tripped, SES sending paused, backup failed, certificate renewal failed, bounce rate over 2 % over 7 days _(the last one not built: see Build notes)_ | P0 |
| O-6 | Build and upgrade: the image is built on the server from the source in the kit (`docker compose build`), with Go dependencies vendored. The version is compiled into the binary (`mailroom version`). Database migrations run automatically at start, forward-only, with a backup taken first. No update check or phone-home | P0 |
| O-7 | Retention: message rows older than 24 months are aggregated into stats and deleted (configurable). Consent logs and suppressions are never deleted automatically _(deferred: see Build notes)_ | P1 |

### 6.15 Import formats (input from the GHL export step)

Mailroom knows nothing about GHL. The separate GHL export step (§11) produces files in these formats, and Mailroom imports them.

| ID | Requirement | Prio |
|---|---|---|
| I-1 | Contacts CSV (imported via `POST /api/v1/imports`): `email` (required), `first_name`, `last_name`, `status` (`active`, `unsubscribed`, `bounced`, `complained`), `tags` (semicolon-separated), `consent_source`, `consent_date`, plus any `attr.<key>` columns. Documented in `app/docs/import-format.md` with an example file | P0 |
| I-2 | Every imported row gets a consent event with source `import:<consent_source>` and the given date | P0 |
| I-3 | Sequence YAML: name, trigger, exit tags, steps (delay + subject + body file). Documented with an example in `app/docs/sequence-format.md` | P0 |

---

## 7. Data model (SQLite)

```text
contacts        id, email (unique, lowercased), first_name, last_name, attributes JSON,
                status, status_changed_at, created_at, updated_at
tags            id, name (unique)
contact_tags    contact_id, tag_id, added_at, source
consent_events  id, contact_id, type (signup|optin_confirmed|import|admin|resubscribe),
                source, form_id, ip, user_agent, consent_text, created_at
suppressions    email_hash (sha256), reason, created_at          -- survives erase
segments        id, name, filter JSON
forms           id (public slug), name, fields JSON, tags JSON, sequence_id,
                double_optin, consent_text, redirect_url
sequences       id, name, trigger JSON, exit_tags JSON, exit_add_tag, reentry,
                send_window JSON, status (active|paused)
sequence_steps  id, sequence_id, position, delay_seconds, email_id
enrollments     id, sequence_id, contact_id, current_step_id, next_run_at,
                status (active|completed|exited), exit_reason, started_at, updated_at
emails          id, subject, preheader, format (markdown|html), body, updated_at
broadcasts      id, name, email_id, segment_id, status, scheduled_at,
                started_at, finished_at, from_override
messages        id, kind (broadcast|sequence|optin|alert), broadcast_id, enrollment_id,
                step_id, contact_id, status (queued|sending|sent|delivered|bounced|
                complained|failed|skipped), ses_message_id (unique), error,
                queued_at, sent_at
                UNIQUE (broadcast_id, contact_id)  -- no double broadcast sends
                UNIQUE (enrollment_id, step_id)    -- no double drip steps
events          id, message_id, type (delivery|bounce|complaint|click|open|unsubscribe),
                detail JSON, created_at
links           id, url (unique)
clicks          id, link_id, message_id, contact_id (NULL only after erase), created_at
jobs            id, type, status, progress, result JSON, created_by, created_at, finished_at
audit_log       id, actor_type (user|token), actor_id, operation, target, changes JSON, created_at
idempotency     key, token_id, request_hash, response JSON, created_at   -- kept 24 h
users, sessions, api_tokens (name, hash, scope, expires_at, last_used_at),
settings (key/value JSON, incl. the read-only sending domain), schema_migrations
```

---

## 8. Key flows

**Sign-up → drip**
1. Visitor submits the form. `POST /f/{id}` validates, rate-limits and checks suppression.
2. Mailroom upserts the contact and writes a consent event.
3. If double opt-in is on, it sends a confirmation email and waits for the click.
4. It applies the form's tags and creates the enrollment (`next_run_at = now + step1.delay`).
5. The scheduler finds due enrollments. For each one, it checks the exit conditions, writes a `messages` row and advances the enrollment **in one transaction**.
6. The sender takes `queued` messages through the token bucket, does the final suppression check, calls SES and stores the MessageId.

**Broadcast**
1. The owner picks an email and a segment, then clicks Send or Schedule.
2. When the broadcast starts, the audience is snapshotted into `queued` messages.
3. The sender works through the queue.
4. The circuit breaker watches the bounce and complaint rates.
5. When no `queued` messages are left, the status becomes `sent`.

**Bounce**
1. SES publishes to SNS, which calls `POST /hooks/ses`.
2. Mailroom verifies the signature and topic, then looks up the message by SES MessageId.
3. It writes the event, updates the message, and applies the X-4 rule to the contact.
4. The contact's active enrollments exit with reason `bounced`.

---

## 9. What we take from listmonk, and what we do differently

listmonk is AGPL-3.0 and Mailroom is MIT, so **no listmonk code may be copied**. We borrow design ideas only.

| listmonk | Mailroom |
|---|---|
| Global blocklist kept separate from per-list subscription status (`schema.sql:1-14`) | Keep the idea (global status + suppression), drop lists in favour of tags |
| Campaign progress as a keyset checkpoint (`last_subscriber_id`), at-most-once on crash (`queries/campaigns.sql:318`) | One `messages` row per recipient with unique constraints: exact status, exactly-once, simple resume. Fine at our scale |
| Rate limit is per worker, so real throughput is workers × rate (`internal/manager/manager.go:492`) | One global token bucket derived from the SES quota |
| Transactional API skips the blocklist and has no unsubscribe (`cmd/tx.go`) | No bypass path. Every message goes through the same suppression check |
| SNS verification supports only SignatureVersion 1 (SHA1), with no TopicArn pinning (`internal/bounce/webhooks/ses.go:213`), and checks for the wrong `UnsubscriptionConfirmation` type (`:93`) | Support v1 + v2, pin the TopicArn, use the correct message types |
| SES bounces read only `destination[0]` and map to campaigns via a header | Map via SES MessageId, and handle all recipients |
| Bounce thresholds are lifetime counts | Transient bounces are counted in a 30-day window |
| Unsubscribe URL = unsigned campaign UUID + subscriber UUID, and the same UUID unlocks data export (`cmd/handlers.go:271`) | HMAC-signed per-message token that only allows unsubscribe |
| Link tracking is opt-in per link (`{{ TrackLink }}`) | Rewrite all links automatically, except unsubscribe links |
| Go `html/template` + Sprig with dangerous functions removed | `html/template` with a small, fixed function set |
| Importer never resurrects blocklisted subscribers (`internal/subimporter`) | Keep this, and extend it to erased (hashed) addresses |
| Double opt-in via an opt-in campaign type | Opt-in is a system email sent on sign-up, and the form only takes effect after confirmation |

---

## 10. Architecture and stack

| Choice | Why |
|---|---|
| **Go** (1.2x), one static binary | Low memory, simple deploy, strong standard library, and the same language as listmonk for reference |
| **SQLite** via `modernc.org/sqlite` (pure Go, no CGO), WAL mode | One file, no DB server, trivial backups. Enough for 50k contacts / 200k emails a month with one writer |
| **html/template + htmx**, assets embedded with `embed` | No frontend build pipeline, and easy for a coding agent to change |
| **Huma v2** (OpenAPI 3.1 generated from code, with request validation) | The spec can't drift from the code, and agents get an exact contract |
| **autocert** (`golang.org/x/crypto/acme/autocert`) | Built-in HTTPS, so no separate reverse proxy, with fewer dependencies than certmagic |
| **AWS SDK for Go v2** (SESv2, SNS, IAM, S3) | Sending, plus the `provision` commands |
| **Cloudflare API** (plain HTTP client) | `provision dns` |
| Background workers in-process: scheduler (60 s), sender, event processor, backup, cleanup | No queue service needed. All state is in SQLite |

**Footprint targets:** image under ~60 MB (changed during the build, see Build notes), idle RAM under 80 MB, under 200 MB while sending.

**Distribution:** the source ships in the kit (`app/`), with Go dependencies vendored and a multi-stage Dockerfile. The client's agent builds the image on the server. No container registry, and no dependency on {{AGENCY}} after handover.

---

## 11. Kit integration

**Folder layout**

```text
ghl-exit-kit/
  LICENSE            MIT, covers the whole kit
  README.md, AGENTS.md, CLAUDE.md, .env.example
  guide/             client step-by-step + screenshots
  ghl-export/        client's GHL data and workflow notes
  tools/ghl-export/  separate GHL export step (not part of Mailroom, not covered here)
  app/               Mailroom source, Dockerfile, OpenAPI spec, docs, this PRD
  deploy/            docker-compose.yml for the server (builds from app/)
```

**What the client's coding agent does** (see AGENTS.md)
1. Preflight.
2. The GHL export step (separate).
3. Create the Hetzner server and install Docker.
4. Build Mailroom on the server, and run `mailroom provision ses`.
5. DNS: `mailroom provision dns --cloudflare`, or hand the client the table from `mailroom dns records`.
6. Start Mailroom (HTTPS, SNS webhook), and bootstrap the admin user and API token.
7. Everything after that goes through the API: settings, layout, import, sequences, enrolments, forms, tests.
8. `doctor` passes, test sends, handover.

No n8n, Postgres or Caddy. The client checkpoints stay as they are.

**IAM for setup (guide step 2.6).** The `setup-agent` user needs AmazonSESFullAccess, AmazonSNSFullAccess and an inline policy that allows:
- creating the user `mailroom-sender` and its policy and access key,
- creating and configuring the bucket `mailroom-backup-*`.

Because that inline policy can set any policy on `mailroom-sender` and create its keys, the setup key is as powerful as an admin key until the customer deactivates it (guide step 7.3); the guide says so. A permissions boundary would close this, but needs a customer-made managed policy, so it isn't used.

The sending user `mailroom-sender` gets only:
- `ses:SendRawEmail`/`ses:SendEmail` on the identity,
- `ses:GetAccount`, and `ses:GetEmailIdentity` on the identity (for `doctor`),
- `sns:Subscribe`/`sns:ListSubscriptionsByTopic` on its topic,
- `s3:PutObject`/`s3:ListBucket` on its backup bucket.

## 12. Non-functional requirements

| Area | Requirement |
|---|---|
| Reliability | No double sends (unique constraints). No lost drip steps across restarts. After a restart, broadcasts resume within 60 s |
| Performance | A 50k segment count in under 1 s. A 50k broadcast snapshot in under 10 s. Sending limited only by the SES rate |
| Security | All public endpoints rate-limited. Secrets only in env/`.env` (0600). Security headers and CSP on the admin UI. Admin can optionally be restricted to IP ranges. Dependencies scanned in CI (govulncheck) |
| Privacy | Hosted in the EU (Hetzner + SES in an EU region). Minimal data. Per-contact click tracking, disclosed in the form consent text (K-4) and switchable off. Export and erase per contact |
| API | The OpenAPI spec is validated in CI. The UI/API parity test (G-1) runs in CI. Every endpoint has at least one integration test |
| Testability | Core logic unit-tested. SES and SNS behind interfaces with fakes. An end-to-end test runs a sequence with shortened delays against a fake SES, and a real smoke test uses the SES mailbox simulator |
| Handover | No ORM (plain SQL next to the code). Vendored dependencies. `app/README.md` explains the code layout so a client's coding agent can change or upgrade it later |

---

## 13. Milestones

| Milestone | Contents | Done when |
|---|---|---|
| **M1: Core** | API foundation (OpenAPI spec, tokens, errors, idempotency, ETags, jobs, audit), contacts, tags, suppression, SES sending, messages, SNS webhook, unsubscribe, base layout, CLI (`provision`, `doctor`, bootstrap), backups | A test email passes DKIM/SPF/DMARC, and bounce and complaint via the simulator suppress the contact |
| **M2: Drips + forms** | Forms, double opt-in, sequences, scheduler, sequence YAML import/export, enrol-at-step, all via API | The community drip (5 emails / 7 days) runs end-to-end with shortened delays and exits on the `customer` tag |
| **M3: Broadcasts + UI** | Segments, broadcasts, throttling, circuit breaker, stats, per-contact click tracking, full admin UI, UI/API parity test, `app/docs/api.md` | A 5k broadcast to the simulator, with pause/resume and a restart mid-send, has no duplicates |
| **M4: Kit** | `deploy/docker-compose.yml`, import and sequence format docs, `app/README.md`, final pass on AGENTS.md and the guide | {{AGENCY}}'s own account is migrated with the kit, followed by one pilot client. Then the kit is frozen and handed over |

---

## 14. Risks

| Risk | Mitigation |
|---|---|
| **No one maintains it after handover.** Security fixes, Go and base-image updates, and any SES API changes are the client's own job | Say so clearly in the README. Keep the code small with few, vendored dependencies. `app/README.md` explains how a coding agent updates Go, dependencies and the base image. The SES v2 API is stable |
| Clients' deliverability suffers from an old GHL list | Clean the list at import, circuit breaker, staged first send (guide) |
| A bug emails unsubscribed people (legal exposure) | One send path, a final suppression check before the SES call, and tests for every suppression case |
| SQLite corruption or a lost disk | WAL mode, nightly `VACUUM INTO` + S3, Hetzner backups, tested restore |
| A coding agent misconfigures the install or overwrites the client's changes | Deterministic `provision` + `doctor` commands, dry runs, `If-Match` on updates, and the audit log |

## 15. Decisions made

- One-off software, handed over as-is. No maintenance, updates or support from {{AGENCY}}.
- Licence: **MIT** (open source). Because of this, no listmonk (AGPL) code is copied.
- The GHL export is a separate step and not part of Mailroom.
- English UI only.
- Double opt-in is off by default and can be switched on per form.
- Click tracking is per contact.
- Exactly one sending domain per install.
- 100 % API-enabled: everything in the app can be read and changed through the REST API.

## 16. Open questions

None at the moment.

---

## 17. Build notes

This section lists the decisions that changed during the build. The requirements above are left as written. Where a note applies, the note wins. [ARCHITECTURE.md](ARCHITECTURE.md) §10 has the full list of implementation decisions.

**Changed requirements**

| Ref | Change |
|---|---|
| E-3, §9 | **Own merge engine instead of `html/template`.** Merge fields are parsed by a small engine in `internal/content`, not rendered as a Go template. The fields are `{{first_name}}`, `{{last_name}}`, `{{email}}`, `{{attr.<key>}}` and `{{unsubscribe_url}}`, each with an optional `\| "fallback"`. Unknown fields, including GHL's `{{contact.*}}`, are rejected when an email is saved. There are no functions at all, which is stricter than "a small fixed function set". The admin UI still uses `html/template` |
| O-7 | **Deferred.** Message rows are never aggregated or deleted automatically. Retention only empties raw event payloads after 90 days, deletes finished jobs after 30 days and deletes expired sessions |
| X-5 | Raw events are not deleted after 90 days. Their payload (`events.detail`) is emptied and the row stays, so stats and the contact timeline keep working |
| B-2, §8 | **The sender completes broadcasts.** The sending package sets `sent` once the snapshot is done and no message of the broadcast is queued or sending. No API call sets it. Without SES credentials the sender doesn't run, so a started broadcast stays `sending` |
| B-7, S-10 | **Test-send marker.** Test sends are messages of kind `test`, marked `{"test": true}` in their data. They never get a working unsubscribe link or tracking, don't count in stats. A broadcast's test send uses the broadcast's sender overrides |
| §6.7 D-6, goal 3 | **Test sends go only to active or pending contacts.** At send time a test is skipped when its address is in the suppressions table or belongs to a contact that is unsubscribed, bounced, complained or manually suppressed. An address with no contact is allowed |
| C-2 | A sixth status, **`pending`**, exists for double opt-in and forced resubscribe confirmations. Pending contacts receive only opt-in emails and tests |
| U-3, U-4 | **`unsubscribe_requested_at` marker.** An unsubscribe from a contact that is bounced, complained or manually suppressed can't change its status, so it is recorded on the contact instead. If an admin later lifts the bounce or the manual suppression, the contact becomes `unsubscribed`, not `active`. "Only stop these emails" also stops an already queued last step and blocks re-entry into that sequence |
| F-6, F-7 | **Existing active contacts on the public form.** A double opt-in form also asks an existing active contact to confirm, and applies tags, the sequence and consent only after the click. A form (single or double opt-in) never overwrites a stored name or attribute; it only fills in missing ones. The endpoint needs no login, so anyone who knows the address could otherwise rewrite what real mail merges in. A pending opt-in's payload is replaced only when a new confirmation email goes out |
| E-1, E-4 | A custom HTML layout must place `{{footer}}` (outside comments) as well as `{{content}}` and an `{{unsubscribe_url}}` link, both visible (not inside an element hidden with the `hidden` attribute, `display:none`, `visibility:hidden`, `font-size:0`, `opacity:0`, `mso-hide:all` or a zero height), so the company name and postal address are in every marketing email. A layout saved before this rule gets the footer appended after the content |
| G-4 | **Read-only POST previews.** A `read` token may also call the POSTs that only compute something and store nothing: `preview-segment`, `render-email`, `preview-email` and `preview-layout`. Every other POST, PUT, PATCH and DELETE needs `admin` |
| S-3 | **The exit tag is added only on automatic exits.** `exit_add_tag` is added when a contact leaves a sequence because of an exit tag or because it is no longer active. A manual unenrol (API or UI) adds no tag, so an admin removing someone doesn't chain them into another tag-triggered sequence |
| B-1, §7 | **Per-broadcast sender: display name and reply-to only.** A broadcast can override `from_name` and `reply_to`; the From address is always `settings.sender.from_email`. There is no `from_override` column |
| X-4 | Only a Permanent bounce marks the message `bounced`, so "bounced" in stats means a hard bounce. Transient bounces are counted toward the 3-in-30-days rule |
| O-5 | The alert for "bounce rate over 2 % over 7 days" was **not built**. The dashboard highlights a 30-day hard-bounce rate from 2 %, and the circuit breaker (B-6) guards each broadcast. The other alerts were built: circuit breaker, SES sending paused, rejected credentials, backup failed or stale, certificate check failed, low disk space, SNS subscription removed and failed broadcast snapshot |
| §10 footprint | The static binary is about 43 MB, mostly the AWS SDK for Go v2. The image target is therefore **under ~60 MB** instead of under 40 MB |
| O-6, goal 7 | **`vendor/` is committed** for reproducible offline builds. It is about 150 MB, because `modernc.org/sqlite` ships generated code for every platform. After changing go.mod, run `go mod vendor` (see `app/README.md`) |

**Not built, or built differently**

| Ref | Status |
|---|---|
| G-10 | Outbound webhooks (P2): not built |
| §6.12, G-1 | Messages, events and clicks each have list and get (`get-event`, `get-click`). Two rows of the coverage table are narrower on purpose: enrolment is bulk (`enroll-sequence-contacts`, with `dry_run`), but unenrolment is one enrollment at a time (`unenroll-sequence-contact`); and API tokens are created, read and revoked but never updated (revoke and create a new one: a token's scope and expiry never change under its holder) |
| §12 Testability | The real smoke test against the SES mailbox simulator (and M1's "bounce and complaint via the simulator suppress the contact") is a documented manual procedure, not a script: it needs live AWS credentials and a public HTTPS endpoint for SNS. AGENTS.md phases 8 and 10 run it (sends to `success@`, `bounce@` and `complaint@simulator.amazonses.com`, then check the contacts become `bounced` and `complained`). The automated tests cover the same flow with a fake provider and signed SNS notifications (`internal/app`) |
| §11 guide | `guide/steps.md` has the text of every step and marks each screenshot (📸) to take; the screenshots themselves are not captured yet. `guide/CAPTURE.md` is the brief for capturing them, and the kit README says so |
| B-10 | Web archive (P2): built at `/archive/{id}`, only for broadcasts to everyone, with no personal data or tracking |
| F-5 | The optional disposable-domain list was not built. The honeypot, per-IP rate limit and syntax check were |
| §12 Security | Rate limits: every public endpoint is limited per client address except the unsubscribe POST (mail providers send one-click unsubscribes from shared addresses, and none may be refused) and the SNS webhook (a 429 would drop SES events); both check an HMAC token or the SNS signature first (ARCHITECTURE.md §7). Restricting the admin UI to IP ranges was not built: use the server firewall or a reverse proxy. The kit has no CI. The spec check and the parity test are part of the test suite, and govulncheck is run by hand (`app/README.md`) |
| O-3 | `mailroom restore` is covered by the test suite (`cmd/mailroom`) rather than a CI job |
| L-6 | `mailroom restore` leaves all sending paused (resume through the API or UI), and carries opt-outs, suppressions, GDPR erasures, already-sent messages and enrollment progress over from the database it replaces, so a restore never re-emails someone who opted out or sends a message twice. Without a database to replace (a new server) it warns that these are lost |
| §7 | The data model grew during the build, e.g. `pending_optins`, `form_submissions`, `contact_status_changes`, `opens`, `media`, `version` columns for ETags, and `test` as a message kind. Forms have an integer `id` plus a random `public_id` for their public URLs. `app/internal/db/migrations/` is the source of truth (`0001_init.sql`, plus `0002_autoincrement_ids.sql`, which makes segment, email, broadcast and sequence-step IDs never reused) |
| I-1 | An import dry run returns its report directly (200). Only the commit runs as a job (202) |
| L-1 | `provision ses` also creates the optional S3 backup bucket (`--no-backup-bucket` skips it) and can rotate the sending key (`--rotate-keys`). The bucket is versioned (overwritten versions of backups expire with them), and the sending user may subscribe only Mailroom's own webhook (`sns:Protocol`/`sns:Endpoint` condition); the doctor warns about any other subscription to the event topic |
