# Mailroom API guide for coding agents

This guide is for an AI coding agent (or a developer) who runs Mailroom through its REST API. It covers how to authenticate, the conventions every endpoint follows, and copy-paste `curl` recipes for the common jobs.

- **Reference:** the OpenAPI 3.1 spec at `/api/v1/openapi.json` (also `/api/v1/openapi.yaml`). Every operation has a description, an example and its required scope. The spec and this guide (`/api/v1/docs`) can be fetched without a token.
- **Everything is in the API.** Every record, setting and action the admin UI has is available under `/api/v1/`. The CLI is only for provisioning and for bootstrapping the first user and token.
- **Formats:** the CSV import format is in [import-format.md](import-format.md) and the sequence YAML format in [sequence-format.md](sequence-format.md). Both files are in `app/docs/` of the kit.

## 0. Setup for the recipes

```bash
export MAILROOM="https://newsletter.example.com"   # https://APP_SUBDOMAIN.DOMAIN, no trailing slash
export TOKEN="$(cat secrets/api-token.txt)"          # an API token, "mr_..."

# Check that the token works and what it may do
curl -sS "$MAILROOM/api/v1/me" -H "Authorization: Bearer $TOKEN"
```

The recipes use `jq` to pick fields out of responses. Without `jq`, read the JSON yourself. Placeholders such as `$CID` (contact ID), `$SID` (sequence ID) or `$BID` (broadcast ID) are values from an earlier response. Add `-i` to any `curl` to see the status code and headers.

Two helpers are used below. Define them once per shell:

```bash
# etag PATH: prints the ETag of a resource, e.g. etag "/api/v1/broadcasts/7" -> "v3"
etag() {
  curl -sS -o /dev/null -D - "$MAILROOM$1" -H "Authorization: Bearer $TOKEN" \
    | tr -d '\r' | awk 'tolower($1) == "etag:" { print $2 }'
}

# wait_job ID: polls a background job until it has succeeded or failed, then prints it
wait_job() {
  while :; do
    job=$(curl -sS "$MAILROOM/api/v1/jobs/$1" -H "Authorization: Bearer $TOKEN")
    case "$(printf '%s' "$job" | jq -r .status)" in
      succeeded|failed) printf '%s\n' "$job"; return ;;
    esac
    sleep 2
  done
}
```

## 1. Authentication

Send an API token on every request:

```text
Authorization: Bearer mr_...
```

- **Scopes.** A token has scope `admin` (everything) or `read` (read operations only). GET operations need `read` or `admin`. Everything else needs `admin`, except a few POSTs that only compute something and change nothing: `POST /api/v1/segments/preview`, `POST /api/v1/emails/render`, `POST /api/v1/emails/{id}/preview` and `POST /api/v1/layout/preview`. Each operation in the spec has its scope in `x-mailroom-scope` and at the end of its description.
- **The first token** comes from the CLI on the server, because there is no token yet to call the API with. The secret is printed once:

  ```bash
  sudo docker compose exec mailroom mailroom token create --name setup-agent --scope admin
  # optional: --expires-in 90d (or 12h), --json for {"token": {...}, "secret": "mr_..."}
  ```

- **Further tokens** are managed through the API (recipe 14): `POST /api/v1/tokens`, `GET /api/v1/tokens`, `DELETE /api/v1/tokens/{id}`. Tokens are stored hashed, so a lost secret can't be recovered: revoke the token and create a new one. A token can have an expiry (`expires_at`), and `last_used_at` shows when it was last used. `created_by` names who created it (`user:<id>` for an admin UI user, `token:<id>`, `cli`). Deleting a user (`DELETE /api/v1/users/{id}`) also revokes the tokens that user created; a password change does not, so revoke those by hand if the user's access should end.
- **Failures.** A missing, malformed, unknown, expired or revoked token is `401 unauthorized`. A `read` token calling a write operation is `403 forbidden`.
- The admin UI (at `/admin/`) signs in with a session cookie plus an `X-CSRF-Token` header. That is for browsers. Agents use tokens.

## 2. Conventions

### JSON, times and IDs

- Requests and responses are JSON. **Send `Content-Type: application/json` with every JSON body.** `curl -d` sends form encoding by default, and the API refuses that (415, code `validation`). The CSV import also accepts `text/csv` and `multipart/form-data` (recipe 12).
- Times are RFC 3339, in UTC in responses (`2026-10-01T08:00:00Z`).
- IDs are integers. Forms also have a random `public_id` for their public URLs.
- `PATCH` changes only the fields you send.
- Unknown JSON fields are rejected with 422, so a typo can't go unnoticed. So are query parameters the operation doesn't take (`field: query.<name>`).

### Errors

Every error has the same shape:

```json
{"error": {"code": "validation", "message": "is not a valid email address", "field": "email"}}
```

`field` is set when the error is about one input: a body field (`email`, `filter.rules[2].value`, `steps[2].delay`), a query parameter (`query.limit`) or a header (`header.If-Match`, `header.Idempotency-Key`). The code set is closed. Branch on `code` rather than the status: `validation` comes with 400, 413, 415 or 422.

| Status | `code` | Typical cause | What to do |
|---|---|---|---|
| 401 | `unauthorized` | No token, or an invalid, expired or revoked one | Check `$TOKEN` |
| 403 | `forbidden` | A `read` token on a write; a status change the suppression rules refuse (e.g. reactivating a complained contact) | Don't retry. Report it |
| 404 | `not_found` | No such record, or no such endpoint or method | Check the ID and the path |
| 409 | `conflict` | A duplicate (email, name), an address that is suppressed or erased, a wrong state (e.g. pausing a broadcast that isn't sending), an Idempotency-Key conflict | Read the current state, then decide |
| 412 | `precondition_failed` | `If-Match` doesn't match: someone changed the record since you read it | Read it again, redo your change, send the new ETag |
| 400 | `validation` | Malformed JSON, or a body that can't be read | Fix the request body |
| 413 | `validation` | The body is too large for the operation | Send less (e.g. split a list), or use the operation meant for large data (CSV import) |
| 415 | `validation` | A content type the operation doesn't take, e.g. form encoding instead of JSON | Send `Content-Type: application/json` (or the type the operation names) |
| 422 | `validation` | Invalid input: a field fails a rule | Fix the input named in `field` |
| 429 | `rate_limited` | Too many requests | Wait `Retry-After` seconds |
| 500 | `internal` | A server error (details are in the server log only) | Retry later with the same Idempotency-Key (a 409 then means the first attempt went through) |

### Pagination

List endpoints take `?limit=` (1-500, default 50) and `?cursor=`, and answer `{"items": [...], "next_cursor": "..."}`. `next_cursor` is empty on the last page. Filters are query parameters, e.g. `GET /api/v1/contacts?status=active&tag=newsletter`.

```bash
cursor=""
while :; do
  page=$(curl -sS "$MAILROOM/api/v1/contacts?status=active&limit=500&cursor=$cursor" \
    -H "Authorization: Bearer $TOKEN")
  printf '%s' "$page" | jq -r '.items[].email'
  cursor=$(printf '%s' "$page" | jq -r .next_cursor)
  [ -z "$cursor" ] && break
done
```

To count instead of listing, see recipe 4 (`POST /api/v1/segments/preview` returns counts).

### ETag and If-Match (safe updates)

A single-resource GET returns an `ETag` header such as `"v3"`, the record's `version` (creates and updates of versioned records return the new one too). Send it back as `If-Match` on `PATCH`, `PUT` and `DELETE`, and on state-changing POSTs that accept it (broadcast send, schedule, pause, resume and cancel; contact status; sequence pause, resume and step changes). If the record changed in the meantime, for example in the UI, you get `412 precondition_failed` instead of overwriting that change. `If-Match` is optional, but an agent should always send it on updates.

```bash
ETAG=$(etag "/api/v1/contacts/$CID")
curl -sS -X PATCH "$MAILROOM/api/v1/contacts/$CID" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: $ETAG" \
  -d '{"first_name": "Alice", "attributes": {"plan": "pro"}}'
```

The value may be `"v3"` (with the quotes), `W/"v3"` or `v3`. Sequence steps use the ETag of their sequence.

### Idempotency-Key (safe retries)

Send an `Idempotency-Key` header on every POST that changes something. A retry with the same key and the same request gets the stored response again (with the header `Idempotent-Replayed: true`) instead of doing the work twice. Keys are kept for 24 hours per token. A key is 1-255 visible ASCII characters. Make it describe the action, e.g. `create-contact-alice-1`.

- The same key with a **different** request (method, path, query or body) is `422` with `field: header.Idempotency-Key`. This also applies after a dry run: a dry run with key `k` stores its answer under `k`, so **use a different key (or none) for the dry run** than for the real request.
- A request that is still running with the same key is `409`. Retry after a moment.
- A request that failed without changing anything can be sent again with the same key only **unchanged** (byte for byte): that is for retries after a `5xx`, a `429` or a timeout. After a `400` or `422` you fix the request, and a fixed request is a different request: send it with a **new** key, or it is `422` on `header.Idempotency-Key`.
- `POST /api/v1/tokens` and `POST /api/v1/users` return a one-time secret, so their response is never stored: a retry with the same key is `409` (the token was created: list the tokens instead of retrying).
- `curl -F` (multipart) picks a new random boundary on every run, so a retried multipart upload is a different request (422). For a retryable CSV import, send the file as `text/csv` (recipe 12).

### Dry runs

`?dry_run=true` runs the operation in a transaction that is rolled back and returns what would have happened, e.g. the recipient count of a broadcast or the report of an import. Nothing changes and no job is created. It is offered on imports, bulk operations, erase, broadcast send and some creates and updates: `POST /api/v1/imports`, `POST /api/v1/tags/bulk`, `DELETE /api/v1/contacts/{id}`, `POST /api/v1/broadcasts/{id}/send`, `POST /api/v1/sequences/import`, `POST /api/v1/sequences/{id}/enrollments`, `POST /api/v1/broadcasts`, `POST /api/v1/emails`, `POST /api/v1/segments`, `POST /api/v1/forms`, `PUT /api/v1/layout`, `POST /api/v1/sending/pause` and others (the spec lists `dry_run` on each). Many writes have none, e.g. `POST /api/v1/contacts`, `POST /api/v1/contacts/upsert`, contact tags and status, `PATCH /api/v1/settings`, suppressions, tags, tokens, users, and sequence create and steps. An operation refuses every query parameter it doesn't list, like an unknown JSON field: `?dry_run=true` on an operation without dry runs, or a misspelling such as `?dryrun=true`, is `422` with `field: query.dry_run` (or `query.dryrun`) and changes nothing. Use a dry run before every import, bulk change, erase and send.

A dry run answers **200** even where the real call answers 201 or 202, and never with an `ETag` or `Location` (also on updates such as `PUT /api/v1/layout`: the version it would name was never stored). What it returns is hypothetical: a dry-run create shows the resource as it would be, but its `id`, `public_id`, `hosted_url` and the like don't exist and are not reserved (a later create may get them), so never keep or link to them. The broadcast in a send dry run is returned unchanged, as stored.

### Jobs and polling

Long work answers `202 Accepted` with a job: imports, broadcast snapshots, backups, bulk tagging, and enrolments of more than 500 contacts. Poll `GET /api/v1/jobs/{id}` (or use `wait_job`) until `status` is `succeeded` or `failed`:

```json
{"id": 17, "type": "tags.bulk", "status": "running", "progress": 0.42, "params": {"tag": "vip", "action": "add", "segment_id": 3}, "result": {}, "error": "", "created_by": "token:3", "created_at": "2026-10-01T08:00:00Z"}
```

- `status` is `queued`, `running`, `succeeded` or `failed`. `progress` goes from 0 to 1. `result` holds the outcome once the job has finished (it may be partial on failure) and `error` says why a job failed.
- A job cut short by a restart fails with error `interrupted`, except resumable ones (such as broadcast snapshots), which run again. Queued jobs survive a restart.
- `GET /api/v1/jobs?type=import.contacts&status=failed` lists jobs.

### Rate limits

Each token has a bucket of 20 requests per second with bursts of 60. Every response carries `RateLimit-Limit`, `RateLimit-Remaining` and `RateLimit-Reset` (seconds). Over the limit you get `429 rate_limited` with `Retry-After`. Wait that long and retry with the same Idempotency-Key. Parallel agents that share a token share its bucket. Unauthenticated requests are limited per client address.

### Audit log

Every write through the API or the UI is logged with the actor, time, operation and changed fields: `GET /api/v1/audit?actor=setup-agent&since=2026-10-01T00:00:00Z`.

## 3. Recipes

### 1. Create a contact, or upsert it by email

Create (409 `conflict` if the email exists, 409 with "address is suppressed" if it was erased or is suppressed):

```bash
curl -sS -X POST "$MAILROOM/api/v1/contacts" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: create-contact-alice-1" \
  -d '{
    "email": "alice@example.com",
    "first_name": "Alice",
    "last_name": "Smith",
    "attributes": {"company": "Acme"},
    "tags": ["newsletter"],
    "consent": {"source": "webinar-2026", "text": "Yes, send me the newsletter. Clicks are tracked per person."}
  }'
```

`status` defaults to `active`. For a migrated contact who opted out, send `"status": "unsubscribed"`. The response (201) is the contact with its `id` and `version`, plus an `ETag`.

Upsert: it creates the contact if the email is new (active, with a consent event), and otherwise updates the names you send, merges `attributes` (a `null` value removes a key) and adds `tags`. The status of an existing contact never changes. The response (200) is `{"contact": {...}, "created": true|false}`:

```bash
curl -sS -X POST "$MAILROOM/api/v1/contacts/upsert" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: upsert-alice-2026-10-01" \
  -d '{"email": "alice@example.com", "first_name": "Alice", "attributes": {"plan": "pro"}, "tags": ["customer"]}'
```

Find a contact by email (`q` matches email or name, so compare the email exactly):

```bash
CID=$(curl -sS -G "$MAILROOM/api/v1/contacts" -H "Authorization: Bearer $TOKEN" \
  --data-urlencode "q=alice@example.com" \
  | jq -r '.items[] | select(.email == "alice@example.com") | .id')
```

Other contact operations:

```bash
# Add tags; a new tag can start a sequence (trigger tag) or end one (exit tag)
curl -sS -X POST "$MAILROOM/api/v1/contacts/$CID/tags" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: tag-$CID-customer" \
  -d '{"tags": ["customer"]}'

# Remove one tag (URL-encode the tag name)
curl -sS -X DELETE "$MAILROOM/api/v1/contacts/$CID/tags/newsletter" -H "Authorization: Bearer $TOKEN"

# Change the status (source api). Reactivating needs a note; complained contacts can never be reactivated (403)
curl -sS -X POST "$MAILROOM/api/v1/contacts/$CID/status" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: $(etag "/api/v1/contacts/$CID")" -H "Idempotency-Key: status-$CID-unsub" \
  -d '{"status": "unsubscribed", "note": "Asked by email"}'

# History: consent, status changes, tags, emails, bounces, clicks
curl -sS "$MAILROOM/api/v1/contacts/$CID/timeline" -H "Authorization: Bearer $TOKEN"
```

To block an address whatever its status, add a manual suppression: `POST /api/v1/suppressions` with `{"email": "...", "note": "..."}`.

### 2. Tag a segment (or a list of contacts) in bulk

```bash
# Dry run: how many contacts match now (200, {"matched": N})
curl -sS -X POST "$MAILROOM/api/v1/tags/bulk?dry_run=true" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"tag": "vip", "action": "add", "segment_id": 3}'

# The real run: 202 with a job of type tags.bulk
JOB=$(curl -sS -X POST "$MAILROOM/api/v1/tags/bulk" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: bulk-vip-segment-3" \
  -d '{"tag": "vip", "action": "add", "segment_id": 3}' | jq -r .job.id)
wait_job "$JOB"   # result: changed, unchanged, missing, failed
```

Use `"contact_ids": [12, 15]` instead of `segment_id` for given contacts, and `"action": "remove"` to take the tag off. The segment is evaluated when the job runs. Adding a tag fires tag triggers like a single add does.

### 3. Export and erase a contact (GDPR)

Export everything held about the contact (contact, tags, consent log, status history, messages to it or its address with their events, other SES events of the address, clicks, opens, enrollments, confirmation requests with what was submitted, and form submissions) as one JSON document. It covers everything erase removes or anonymises:

```bash
curl -sS "$MAILROOM/api/v1/contacts/$CID/export" -H "Authorization: Bearer $TOKEN" -o "contact-$CID.json"
```

Erase deletes the contact and all personal data, anonymises its messages and clicks, and keeps only a SHA-256 hash of the address in the suppressions, so it can't be re-added by an import or a form. **It can't be undone.** Do a dry run, show the counts, and erase only with the owner's explicit OK:

```bash
# Dry run: the same counts, nothing changes
curl -sS -X DELETE "$MAILROOM/api/v1/contacts/$CID?dry_run=true" -H "Authorization: Bearer $TOKEN"

# Erase
curl -sS -X DELETE "$MAILROOM/api/v1/contacts/$CID" \
  -H "Authorization: Bearer $TOKEN" -H "If-Match: $(etag "/api/v1/contacts/$CID")"
```

The response lists what was deleted (`deleted.contact`, `deleted.consent_events`, ...), what was anonymised (`anonymised.messages`, `anonymised.clicks`, ...) and the `email_hash` kept in the suppressions.

### 4. Build a segment and count it

A filter is `{"match": "all"|"any", "rules": [...]}`, and each rule is a condition or a nested group. The conditions (full list in the description of `POST /api/v1/segments` in the spec):

| Condition | Example |
|---|---|
| tag | `{"field": "tag", "op": "has", "value": "customer"}` (`not_has`) |
| several tags | `{"field": "tags", "op": "has_any", "values": ["a", "b"]}` (`has_all`, `has_none`) |
| status | `{"field": "status", "op": "is", "value": "active"}` (`is_not`) |
| email, names | `{"field": "email", "op": "ends_with", "value": "@acme.com"}` (`eq`, `contains`; names also `is_empty`, `not_empty`) |
| created | `{"field": "created_at", "op": "within_days", "days": 30}` (`before`, `after` with an RFC 3339 `value`) |
| attribute | `{"field": "attr", "key": "plan", "op": "eq", "value": "pro"}` (`neq`, `contains`, `exists`, `not_exists`) |
| clicks | `{"field": "clicked", "op": "link", "url": "https://example.com/offer", "within_days": 30}` (`any_link`, `no_click`; optional `broadcast_id`) |

A filter never adds a status condition by itself. Broadcasts always go only to the active members.

```bash
# Live count without saving (read scope is enough): total, active, a sample, missing_tags
curl -sS -X POST "$MAILROOM/api/v1/segments/preview" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "filter": {"match": "all", "rules": [
      {"field": "tag", "op": "has", "value": "customer"},
      {"field": "tag", "op": "not_has", "value": "vip"}
    ]},
    "sample_limit": 5
  }'

# Save it (201; names are unique, ignoring case)
SEG=$(curl -sS -X POST "$MAILROOM/api/v1/segments" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: segment-customers-without-vip" \
  -d '{
    "name": "Customers without VIP",
    "filter": {"match": "all", "rules": [
      {"field": "tag", "op": "has", "value": "customer"},
      {"field": "tag", "op": "not_has", "value": "vip"}
    ]}
  }' | jq -r .id)

# Count it now: {"total": N, "active": M, "missing_tags": [...]}; active = the recipients of a broadcast
curl -sS "$MAILROOM/api/v1/segments/$SEG/count" -H "Authorization: Bearer $TOKEN"

# The members, e.g. only the active ones
curl -sS "$MAILROOM/api/v1/segments/$SEG/members?status=active&limit=100" -H "Authorization: Bearer $TOKEN"
```

`missing_tags` lists tag names the filter refers to that no tag has (a typo, or a tag that was deleted). A `has` rule on such a name matches nobody. The same preview counts contacts by status, e.g. after an import: `{"filter": {"rules": [{"field": "status", "op": "is", "value": "unsubscribed"}]}}`.

### 5. Write and preview an email

Emails are Markdown (default) or raw HTML (`"format": "html"`, e.g. copied from GHL). Every email is wrapped in the base layout with the unsubscribe footer when it is sent. Merge fields: `{{first_name}}`, `{{last_name}}`, `{{email}}`, `{{attr.<key>}}` and `{{unsubscribe_url}}`, each with an optional fallback such as `{{first_name | "there"}}`. Any other merge field is a 422. `GET /api/v1/emails/merge-fields` lists the fields with the attribute keys found on contacts and how many contacts have each one.

HTML bodies that start with `<!-- mailroom:visual -->` were made in the admin UI's visual editor: they get the same inline styles as Markdown output, and a link with `class="mr-button"` is drawn as a button in the layout's brand colour. Other HTML is sent exactly as written.

```bash
# Render without saving (live preview; read scope is enough). contact_id fills in a real contact's data
curl -sS -X POST "$MAILROOM/api/v1/emails/render" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"subject": "Welcome, {{first_name | \"friend\"}}!", "body": "Hi {{first_name | \"there\"}},\n\nThanks for joining.", "contact_id": 42}'

# Send a test of unsaved content (202): to 1-5 addresses (default: the owner), with a contact's data
curl -sS -X POST "$MAILROOM/api/v1/emails/test" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -H "Idempotency-Key: test-draft-1" \
  -d '{"subject": "Welcome!", "body": "Hi {{first_name | \"there\"}}", "to": ["owner@example.com"], "contact_id": 42}'

# Save it (201)
EID=$(curl -sS -X POST "$MAILROOM/api/v1/emails" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: email-welcome-1" \
  -d '{
    "name": "Welcome 1",
    "subject": "Welcome, {{first_name | \"friend\"}}!",
    "preheader": "Here is what happens next",
    "format": "markdown",
    "body": "Hi {{first_name | \"there\"}},\n\nThanks for joining."
  }' | jq -r .id)

# Preview the stored email as a given contact would get it: {"subject", "html", "text"}
curl -sS -X POST "$MAILROOM/api/v1/emails/$EID/preview" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"contact_id": 42}'

# Test send: {} goes to the owner's address; "to" takes 1-5 addresses (202 with message_ids)
curl -sS -X POST "$MAILROOM/api/v1/emails/$EID/test" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: test-email-$EID-1" \
  -d '{"to": ["owner@example.com"], "contact_id": 42}'

# Edit it later (changes apply to every future send, including sequence steps using it)
curl -sS -X PATCH "$MAILROOM/api/v1/emails/$EID" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: $(etag "/api/v1/emails/$EID")" \
  -d '{"subject": "Welcome aboard, {{first_name | \"friend\"}}!"}'
```

Test messages skip suppressed addresses and contacts that are unsubscribed, bounced, complained or suppressed, carry an inactive unsubscribe link and don't count in stats. While the SES account is in the sandbox, send tests only to addresses on the sending domain and to the SES mailbox simulator (`success@simulator.amazonses.com`, `bounce@simulator.amazonses.com`, `complaint@simulator.amazonses.com`; add a label such as `bounce+test1@simulator.amazonses.com` per test, because a bounced or complained address is never emailed again). The base layout is `GET /api/v1/layout` and `PUT /api/v1/layout`, and `POST /api/v1/layout/preview` previews a change. A `custom_html` layout must contain `{{content}}`, a visible link to `{{unsubscribe_url}}` and a visible `{{footer}}` (not in a comment or inside a hidden element such as `display:none`, `visibility:hidden`, `font-size:0` or `opacity:0`; the settings' `company_footer`, with the company name and postal address every marketing email needs), or it is a 422 on `custom_html`. `company_footer` can't be set to an empty text (422). While it is empty (`COMPANY_FOOTER` was never set), `send-broadcast` and `resume-sequence` answer 422 on `company_footer`, broadcast and sequence emails wait in the queue (nothing is lost, the owner is alerted), and the doctor's `config` check fails.

### 6. Create and send a broadcast

A broadcast owns its email. The audience is `{"type": "all"}` (every active contact, the default) or `{"type": "segment", "segment_id": N}` (the active members of that segment). The From address comes from the settings (`GET /api/v1/settings`), and `from_name` and `reply_to` can override it per broadcast (`reply_to` must be on the sending domain).

```bash
# 1. Create the draft (201)
BID=$(curl -sS -X POST "$MAILROOM/api/v1/broadcasts" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: broadcast-october-news" \
  -d '{
    "name": "October newsletter",
    "audience": {"type": "segment", "segment_id": 3},
    "email": {
      "subject": "Our autumn news, {{first_name | \"friend\"}}",
      "preheader": "Three things worth knowing",
      "body": "Hi {{first_name | \"there\"}},\n\nHere is our news."
    }
  }' | jq -r .id)

# 2. Test send to 1-5 addresses, optionally with a real contact's data (202)
curl -sS -X POST "$MAILROOM/api/v1/broadcasts/$BID/test" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: broadcast-$BID-test-1" \
  -d '{"to": ["owner@example.com"], "contact_id": 42}'

# 3. Dry run: the recipient count, nothing changes (200, {"dry_run": true, "recipients": N, "missing_tags": [...]})
curl -sS -X POST "$MAILROOM/api/v1/broadcasts/$BID/send?dry_run=true" -H "Authorization: Bearer $TOKEN"

# 4. Send now (202 with the broadcast, the snapshot job and "recipients")
curl -sS -X POST "$MAILROOM/api/v1/broadcasts/$BID/send" \
  -H "Authorization: Bearer $TOKEN" \
  -H "If-Match: $(etag "/api/v1/broadcasts/$BID")" -H "Idempotency-Key: broadcast-$BID-send"
```

Only send to real recipients once the owner has approved it. Instead of sending now, schedule the draft for a time in the future (`unschedule` moves it back to draft):

```bash
curl -sS -X POST "$MAILROOM/api/v1/broadcasts/$BID/schedule" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: $(etag "/api/v1/broadcasts/$BID")" -H "Idempotency-Key: broadcast-$BID-schedule" \
  -d '{"at": "2026-10-01T08:00:00Z"}'
```

**Watch it.** The status goes `draft` (or `scheduled`) → `sending` → `sent`. The job `broadcast.snapshot` queues one message per active, unsuppressed contact, and `snapshot_done_at` is set when it is done. Contacts who become inactive before their message goes out are skipped.

```bash
# Status, recipients_total, snapshot_done_at, paused_reason
curl -sS "$MAILROOM/api/v1/broadcasts/$BID" -H "Authorization: Bearer $TOKEN" \
  | jq '{status, recipients_total, snapshot_done_at, paused_reason}'

# Numbers: recipients, queued, sent, delivered, bounced, complained, failed, skipped, unsubscribed,
# unique_clicks, clicks_per_link and rates (fractions of sent)
curl -sS "$MAILROOM/api/v1/broadcasts/$BID/stats" -H "Authorization: Bearer $TOKEN"

# The individual messages, e.g. the bounced ones
curl -sS "$MAILROOM/api/v1/broadcasts/$BID/recipients?status=bounced" -H "Authorization: Bearer $TOKEN"
```

The **circuit breaker** pauses a sending broadcast by itself (`paused_reason: circuit_breaker`) when hard bounces go above 5 % or complaints above 0.1 % after the first 200 messages, and alerts the owner. The thresholds are in the settings (`circuit_breaker`). Other operations: `POST /api/v1/broadcasts/{id}/duplicate` makes a new draft from any broadcast, and `PATCH /api/v1/broadcasts/{id}` edits a draft.

### 7. Pause, resume and cancel

```bash
# Pause a sending broadcast (409 unless it is sending); queued messages wait
curl -sS -X POST "$MAILROOM/api/v1/broadcasts/$BID/pause" \
  -H "Authorization: Bearer $TOKEN" -H "If-Match: $(etag "/api/v1/broadcasts/$BID")" \
  -H "Idempotency-Key: broadcast-$BID-pause-1"

# Resume a paused broadcast. After the circuit breaker, check the stats first and send {"confirm": true} (422 without it)
curl -sS -X POST "$MAILROOM/api/v1/broadcasts/$BID/resume" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: $(etag "/api/v1/broadcasts/$BID")" -H "Idempotency-Key: broadcast-$BID-resume-1" \
  -d '{"confirm": true}'

# Cancel a draft, scheduled or paused broadcast (409 while sending: pause it first). Can't be undone
curl -sS -X POST "$MAILROOM/api/v1/broadcasts/$BID/cancel" \
  -H "Authorization: Bearer $TOKEN" -H "If-Match: $(etag "/api/v1/broadcasts/$BID")" \
  -H "Idempotency-Key: broadcast-$BID-cancel"
```

Stop **all** sending (broadcasts, sequence steps, opt-in confirmations, tests, alerts) until a resume. Queued messages are kept:

```bash
curl -sS -X POST "$MAILROOM/api/v1/sending/pause" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: sending-pause-1" \
  -d '{"reason": "Checking the list before the next batch"}'
curl -sS "$MAILROOM/api/v1/sending/state" -H "Authorization: Bearer $TOKEN"
curl -sS -X POST "$MAILROOM/api/v1/sending/resume" \
  -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: sending-resume-1"
```

Sending also pauses by itself when SES pauses the account (`source: ses`), and resumes when SES enables it again. A single sequence is paused with `POST /api/v1/sequences/{id}/pause` and resumed with `POST /api/v1/sequences/{id}/resume`. Enrolments keep their place while it is paused.

### 8. Create a sequence from YAML

The format, with every field, is in [sequence-format.md](sequence-format.md). The request carries the YAML as a string and the body files by name. With files on disk:

```bash
# Dry run: everything is validated; 200 with the sequence that would be created
curl -sS -X POST "$MAILROOM/api/v1/sequences/import?dry_run=true" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  --data @- <<EOF
{
  "yaml": $(jq -Rs . < community-welcome.yaml),
  "files": {
    "01-welcome.md": $(jq -Rs . < 01-welcome.md),
    "02-first-win.md": $(jq -Rs . < 02-first-win.md)
  }
}
EOF
```

Then send the same request without `?dry_run=true` and with `-H "Idempotency-Key: import-community-welcome-1"`: 201 with the sequence, its steps (each with `id`, `position`, `delay_seconds` and `email_id`) and an `ETag`. A small sequence can have its bodies inline in the YAML:

```bash
curl -sS -X POST "$MAILROOM/api/v1/sequences/import" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: import-welcome-drip-1" \
  --data @- <<'EOF'
{
  "yaml": "name: Welcome drip\ntrigger:\n  tag: joined\nexit_tags: [customer]\nreentry: never\nsteps:\n  - delay: \"0\"\n    subject: Welcome!\n    body: |\n      Hi {{first_name | \"there\"}}, thanks for joining.\n  - delay: \"2d\"\n    subject: Your first win\n    body: |\n      Here is one thing to try this week.\n"
}
EOF
```

A 422 names the place, e.g. `field: steps[1].delay`. A name that already exists is 409. `GET /api/v1/sequences/{id}/export` returns the same format (`{"yaml": ..., "files": {...}}`) for version control or copying to another install.

Tags that arrive through a CSV import never start a sequence. Contacts are enrolled by the trigger tag (added later), by a form, or explicitly (recipe 10).

### 9. Change a drip delay (or a step's content)

```bash
# The steps with their IDs and delays, and the sequence's ETag
curl -sS "$MAILROOM/api/v1/sequences/$SID" -H "Authorization: Bearer $TOKEN" \
  | jq '.steps[] | {id, position, delay, subject}'

# Set step $STEP to two days (172800 s). Contacts waiting for this step move by the difference
curl -sS -X PATCH "$MAILROOM/api/v1/sequences/$SID/steps/$STEP" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: $(etag "/api/v1/sequences/$SID")" \
  -d '{"delay_seconds": 172800}'

# Or change its email's subject and body (applies to messages not sent yet)
curl -sS -X PATCH "$MAILROOM/api/v1/sequences/$SID/steps/$STEP" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: $(etag "/api/v1/sequences/$SID")" \
  -d '{"subject": "Two days in: your first win", "body": "Hi {{first_name | \"there\"}}, ..."}'

# Send the step to the owner ({}) or to 1-5 addresses in "to"
curl -sS -X POST "$MAILROOM/api/v1/sequences/$SID/steps/$STEP/test" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: step-$STEP-test-1" \
  -d '{}'
```

`delay_seconds` counts from when the previous step was queued (for the first step: from the enrolment). Step edits need the **sequence's** ETag, and each answer carries the new one. To test a sequence quickly, shorten the delays temporarily and restore them afterwards. Steps are added with `POST /api/v1/sequences/{id}/steps`, reordered with `POST /api/v1/sequences/{id}/steps/reorder` and deleted with `DELETE /api/v1/sequences/{id}/steps/{step_id}` (see the spec for what happens to waiting contacts).

### 10. Enrol contacts at a given step

For contacts who were mid-drip in the old system: `at_step` is the step position to start at (0 or 1 = the first step), and that step is due at `start_at` plus its delay (`start_at` may be in the past). Only active contacts are enrolled, and the sequence's re-entry policy applies.

```bash
# Dry run: would_enroll and skipped by reason (not_found, not_active, already_active, reentry, exit_tag, no_steps, unsubscribed)
curl -sS -X POST "$MAILROOM/api/v1/sequences/$SID/enrollments?dry_run=true" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"emails": ["alice@example.com", "bob@example.com"], "at_step": 3, "start_at": "2026-10-01T08:00:00Z", "source": "migration"}'

# The real run
curl -sS -X POST "$MAILROOM/api/v1/sequences/$SID/enrollments" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: enrol-migration-step3-batch1" \
  -d '{"emails": ["alice@example.com", "bob@example.com"], "at_step": 3, "start_at": "2026-10-01T08:00:00Z", "source": "migration"}'
```

`contact_ids` works instead of `emails`. Up to 500 contacts are enrolled at once (200 with `enrolled` and `enrollment_ids`). More than 500 run as a job (202 with `job`), whose result has the same fields. Check the result:

```bash
curl -sS "$MAILROOM/api/v1/sequences/$SID/enrollments?status=active" -H "Authorization: Bearer $TOKEN"
curl -sS "$MAILROOM/api/v1/sequences/$SID/stats" -H "Authorization: Bearer $TOKEN"   # active/completed/exited, per step sent, delivered, clicked...
```

To take a contact out: `DELETE /api/v1/sequences/{id}/enrollments/{enrollment_id}`. A contact's enrolments are at `GET /api/v1/contacts/{id}/enrollments`.

### 11. Create a sign-up form and get its snippet

```bash
FORM=$(curl -sS -X POST "$MAILROOM/api/v1/forms" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: form-newsletter-1" \
  -d '{
    "name": "Newsletter sign-up",
    "fields": [{"key": "first_name", "label": "First name", "required": false}],
    "tags": ["newsletter"],
    "sequence_id": 2,
    "double_optin": false,
    "redirect_url": "https://example.com/thanks",
    "allowed_origins": ["https://example.com"]
  }')
printf '%s' "$FORM" | jq '{id, public_id, hosted_url, endpoint}'
FID=$(printf '%s' "$FORM" | jq -r .id)

# Copy-paste HTML for the website (with the honeypot field and the consent text), plus the hosted page URL
curl -sS "$MAILROOM/api/v1/forms/$FID/snippet" -H "Authorization: Bearer $TOKEN" | jq -r .html > form-snippet.html
```

- `fields` are inputs besides `email` (always present and required): `first_name`, `last_name` or `attr.<key>`.
- `consent_text` defaults to a text that says signing up means receiving email and that clicks are recorded per person. `double_optin` is off unless you set it. `redirect_url` must be an absolute https URL. `allowed_origins` is only needed for JavaScript submissions (CORS); plain HTML forms work from anywhere.
- The hosted page is `hosted_url` (`https://APP_SUBDOMAIN.DOMAIN/f/{public_id}`), for linking to directly.
- The public endpoint needs no login, so a submission for an address that is already an active contact never overwrites what is stored: it only fills in a name or attribute the contact doesn't have yet. On a single opt-in form it adds the form's tags, the signup consent and the sequence right away (as for a new address). On a double opt-in form nothing changes until the contact clicks the confirmation email, and the click, too, only fills in missing values. Change a subscriber's stored name with `PATCH /api/v1/contacts/{id}`.

Test it once with a test address (the public endpoint needs no token and answers JSON to JSON requests). Then check the outcome and the contact:

```bash
ENDPOINT=$(printf '%s' "$FORM" | jq -r .endpoint)
curl -sS -X POST "$ENDPOINT" -H "Content-Type: application/json" \
  -d '{"email": "success+form1@simulator.amazonses.com", "first_name": "Test"}'

# Outcomes: created, updated, optin_pending, resubscribe_pending, rejected, spam, rate_limited
curl -sS "$MAILROOM/api/v1/forms/$FID/submissions" -H "Authorization: Bearer $TOKEN"
```

### 12. Import contacts from CSV (dry run, then commit)

The file format is in [import-format.md](import-format.md). An import never reactivates a contact, and erased or suppressed addresses are rejected.

```bash
# Dry run: evaluates the whole file now and writes nothing (200 with "report")
curl -sS -X POST "$MAILROOM/api/v1/imports?dry_run=true" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: text/csv" \
  --data-binary @ghl-export/contacts.csv | jq .report
```

Show the report (`rows`, `created`, `updated`, `rejected_count`, `rejected` line numbers with reasons, `status_counts`, `tag_counts`, `warnings`) and get the owner's approval. Then commit:

```bash
# The import itself: 202 with a job of type import.contacts
JOB=$(curl -sS -X POST "$MAILROOM/api/v1/imports?on_existing=update" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: text/csv" \
  -H "Idempotency-Key: import-contacts-2026-10-01" \
  --data-binary @ghl-export/contacts.csv | jq -r .job.id)

# Follow it until status is succeeded or failed; the report is in "report" once it has finished
curl -sS "$MAILROOM/api/v1/imports/$JOB" -H "Authorization: Bearer $TOKEN" | jq '{status, progress, report}'
```

- `on_existing=update` (default) fills in names and attributes and adds tags. `skip` leaves existing contacts as they are. Either way a status that blocks sending is applied.
- `--data-binary` with `text/csv` sends the file byte for byte, so a retry with the same Idempotency-Key is recognised. A multipart upload also works (`-F file=@ghl-export/contacts.csv`), but a retry of it is a new request (see Idempotency-Key above).
- The file can be at most 50 MiB. A file without a header row or `email` column is 422 with `field: file`.
- Past imports: `GET /api/v1/imports`.

### 13. Read doctor, health and SES status

```bash
# The install checks of "mailroom doctor": DNS, SES identity and production access, SNS subscription,
# HTTPS certificate, last backup, clock skew, disk space, sender config. status = the worst check
curl -sS "$MAILROOM/api/v1/system/doctor" -H "Authorization: Bearer $TOKEN" \
  | jq '{status, problems: [.checks[] | select(.status == "warn" or .status == "fail")]}'

# Database, schema version, queue depth, jobs, uptime, version (status ok, degraded or fail)
curl -sS "$MAILROOM/api/v1/system/health" -H "Authorization: Bearer $TOKEN"

# SES account: production access (false = sandbox), sending enabled, quota, quota_used; 409 when AWS isn't configured
curl -sS "$MAILROOM/api/v1/system/ses?refresh=true" -H "Authorization: Bearer $TOKEN"

# Whether all sending is paused, the current send rate and the last SES snapshot
curl -sS "$MAILROOM/api/v1/sending/state" -H "Authorization: Bearer $TOKEN"

# Build version
curl -sS "$MAILROOM/api/v1/system/version" -H "Authorization: Bearer $TOKEN"
```

Each doctor check has `name`, `status` (`ok`, `warn`, `fail`, `skipped`), `message` and a `fix_hint`. The checks take at most 15 seconds. On the server, `mailroom doctor` runs the same checks and exits with 2 when one fails. The unauthenticated liveness probe is `GET /healthz`. The dashboard numbers (new contacts, unsubscribes, sent this month, bounce and complaint rates) are at `GET /api/v1/stats/dashboard`.

### 14. Create an API token

For an integration (e.g. a payment system that adds the tag `customer`) or a read-only reporting agent:

```bash
curl -sS -X POST "$MAILROOM/api/v1/tokens" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: token-payment-integration" \
  -d '{"name": "payment-integration", "scope": "admin", "expires_at": "2027-10-01T00:00:00Z"}'
```

The response (201) is `{"token": {"id", "name", "scope", "prefix", ...}, "secret": "mr_..."}`. **The secret is shown only in this response.** Store it in `secrets/` or give it to its owner, and never write it to chat, logs or commits. Names are unique. Omit `expires_at` for a token that never expires, and use `"scope": "read"` for read-only access.

```bash
curl -sS "$MAILROOM/api/v1/tokens" -H "Authorization: Bearer $TOKEN"                 # list (secrets are never shown again)
curl -sS -X DELETE "$MAILROOM/api/v1/tokens/$TOKEN_ID" -H "Authorization: Bearer $TOKEN"   # revoke (204), works immediately
```

### 15. Trigger a backup

The nightly backup runs at 02:30 in the settings time zone and local backups are kept for 14 days. When `BACKUP_BUCKET` is set, each backup is also uploaded to S3.

```bash
# Start a backup: 202 with a job
JOB=$(curl -sS -X POST "$MAILROOM/api/v1/system/backups" \
  -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: backup-2026-10-01-1" | jq -r .job.id)
wait_job "$JOB"   # result: backup {name, size_bytes, created_at}, path, s3_key, pruned

# The backups and the schedule (next_run_at, retention_days, s3_bucket)
curl -sS "$MAILROOM/api/v1/system/backups" -H "Authorization: Bearer $TOKEN"

# Download one by name
curl -sS "$MAILROOM/api/v1/system/backups/mailroom-20261001T023000Z.db" \
  -H "Authorization: Bearer $TOKEN" -o mailroom-20261001T023000Z.db
```

A failed backup alerts the owner. Restoring is done offline on the server with `mailroom restore FILE` while the server is stopped (see the kit's deploy/README.md). A restored server starts with all sending paused (`GET /api/v1/sending/state` shows a manual pause whose reason starts with "restored from a backup"): check the contacts, broadcasts and sequences, then `POST /api/v1/sending/resume`. The restore carries unsubscribes, bounces, suppressions, GDPR erasures and already-sent messages over from the database it replaced; without one (a new server) they are lost, and the command warns. The key file `data/secret.key` and `data/media/` belong with the backups.

### 16. Read and answer received email (Inbox)

When receiving is set up (`GET /api/v1/inbound/counts` says `enabled: true`), every email sent to an address on the domain is in the Inbox. `pending` messages were received by SES and are being fetched; `failed` ones could not be fetched (`fetch_error`).

```bash
# Unread email, newest first (box: inbox, unread, archived or all; q searches subject and sender)
curl -sS "$MAILROOM/api/v1/inbound?box=unread" -H "Authorization: Bearer $TOKEN"

# One message: text, html (untrusted: show it sandboxed), verdicts, attachment_list, replies_sent
curl -sS "$MAILROOM/api/v1/inbound/12" -H "Authorization: Bearer $TOKEN"
curl -sS "$MAILROOM/api/v1/inbound/12/attachments/1" -H "Authorization: Bearer $TOKEN" -o attachment

# Reply (dry run first: where it goes and the subject), then send it: 202 with message_id
curl -sS -X POST "$MAILROOM/api/v1/inbound/12/reply?dry_run=true" -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" -d '{"body": "Hi Anna,\n\nThanks, see you Friday.\n\nBen"}'
curl -sS -X POST "$MAILROOM/api/v1/inbound/12/reply" -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: reply-12-1" \
  -H "Content-Type: application/json" -d '{"body": "Hi Anna,\n\nThanks, see you Friday.\n\nBen"}'

# Mark read, or archive
curl -sS -X PATCH "$MAILROOM/api/v1/inbound/12" -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" -d '{"read": true, "archived": true}'
```

A reply is a plain personal email from the settings sender (no layout, unsubscribe link or tracking), threaded with the received message and quoting it unless `"quote": false`. It is refused with 409 when the address is suppressed or its contact bounced or complained. Never reply on a person's behalf without being asked to: show the text to a human first.

## 4. Where things are

| Area | Endpoints (all under `/api/v1`) |
|---|---|
| Contacts | `GET/POST /contacts` (look one up by address with `GET /contacts?email=ADDRESS`), `POST /contacts/upsert`, `GET/PATCH/DELETE /contacts/{id}`, `POST /contacts/{id}/status`, `POST /contacts/{id}/tags`, `DELETE /contacts/{id}/tags/{tag}`, `GET /contacts/{id}/timeline`, `GET /contacts/{id}/enrollments`, `GET /contacts/{id}/export` |
| Tags | `GET/POST /tags`, `GET/PATCH/DELETE /tags/{id}` (PATCH renames), `POST /tags/{id}/merge`, `POST /tags/bulk` |
| Suppressions | `GET/POST /suppressions`, `POST /suppressions/remove` (manual suppressions only) |
| Segments | `GET/POST /segments`, `POST /segments/preview`, `GET/PATCH/DELETE /segments/{id}`, `GET /segments/{id}/count`, `GET /segments/{id}/members` |
| Emails | `GET/POST /emails`, `POST /emails/render`, `POST /emails/test` (test of unsaved content), `GET /emails/merge-fields`, `GET/PATCH/DELETE /emails/{id}`, `GET /emails/{id}/usage` (the steps and broadcasts that use it), `POST /emails/{id}/preview`, `POST /emails/{id}/test` |
| Layout and media | `GET/PUT /layout`, `POST /layout/preview`, `GET/POST /media`, `GET/DELETE /media/{id}` |
| Broadcasts | `GET/POST /broadcasts`, `GET/PATCH/DELETE /broadcasts/{id}`, `POST /broadcasts/{id}/test`, `POST /broadcasts/{id}/send`, `POST /broadcasts/{id}/schedule`, `POST /broadcasts/{id}/unschedule`, `POST /broadcasts/{id}/pause`, `POST /broadcasts/{id}/resume`, `POST /broadcasts/{id}/cancel`, `POST /broadcasts/{id}/duplicate`, `GET /broadcasts/{id}/stats`, `GET /broadcasts/{id}/recipients` |
| Sequences | `GET/POST /sequences`, `POST /sequences/import`, `GET/PATCH/DELETE /sequences/{id}`, `GET /sequences/{id}/export`, `GET /sequences/{id}/stats`, `POST /sequences/{id}/pause`, `POST /sequences/{id}/resume` |
| Sequence steps | `POST /sequences/{id}/steps`, `POST /sequences/{id}/steps/reorder`, `PATCH/DELETE /sequences/{id}/steps/{step_id}`, `POST /sequences/{id}/steps/{step_id}/test` |
| Enrolments | `GET/POST /sequences/{id}/enrollments`, `DELETE /sequences/{id}/enrollments/{enrollment_id}` |
| Forms | `GET/POST /forms`, `GET/PATCH/DELETE /forms/{id}`, `GET /forms/{id}/snippet`, `GET /forms/{id}/submissions` |
| Imports | `GET/POST /imports`, `GET /imports/{job_id}` |
| Messages and events | `GET /messages`, `GET /messages/{id}`, `GET /events`, `GET /events/{id}`, `GET /clicks`, `GET /clicks/{id}` |
| Sending | `GET /sending/state`, `POST /sending/pause`, `POST /sending/resume` |
| Settings | `GET/PATCH /settings` (sender, footer, tracking, send rate, circuit breaker, time zone, default send window, alert address; the domain is read-only) |
| Users | `GET /me`, `GET/POST /users`, `GET/PATCH/DELETE /users/{id}`, `POST/DELETE /users/{id}/totp`, `POST /users/{id}/totp/confirm` |
| API tokens | `GET/POST /tokens`, `GET/DELETE /tokens/{id}` |
| System | `GET /system/health`, `GET /system/version`, `GET /system/doctor`, `GET /system/ses`, `GET/POST /system/backups`, `GET /system/backups/{name}`, `GET /stats/dashboard` |
| Jobs and audit | `GET /jobs`, `GET /jobs/{id}`, `GET /audit` |
| Inbox (received email) | `GET /inbound`, `GET /inbound/counts`, `GET/PATCH/DELETE /inbound/{id}`, `GET /inbound/{id}/attachments/{position}`, `POST /inbound/{id}/reply` |

Public endpoints outside the API (no token): `/f/{public_id}` (forms), `/u/...` (unsubscribe), `/c/...` (click redirects), `/o/...` (open pixel), `/hooks/ses` (SES notifications through SNS, including received mail) and `/healthz`.

## 5. Rules of thumb for agents

1. Read before you write: GET the record, keep its ETag, and send it as `If-Match`.
2. Put an `Idempotency-Key` on every POST that changes something. Retry an unchanged request (after a 5xx, 429 or timeout) with the same key; after fixing a request that got a 400 or 422, use a new key. Use a different key for a dry run than for the real run.
3. Dry-run every import, bulk change, erase and send, and show the result to a human before the real run when the change is large or can't be undone.
4. Poll jobs; don't assume that a 202 means the work is done.
5. On 412, read again and redo the change. On 409, read the current state. On 429, wait `Retry-After`. Never loop blindly on 422 or 403.
6. Never send to real recipients without the owner's approval. Test with the SES mailbox simulator and addresses on the sending domain.
7. Keep token secrets out of chat, logs and commits.
