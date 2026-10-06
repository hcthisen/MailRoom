# Contacts CSV import format

Mailroom imports contacts from a CSV file with `POST /api/v1/imports` (PRD I-1, I-2). This page describes the file format, what an import does with each row, and how to read the report. The GHL export step (AGENTS.md phase 1) writes `ghl-export/contacts.csv` in this format.

## Example

```csv
email,first_name,last_name,status,tags,consent_source,consent_date,attr.company,attr.plan
alice@example.com,Alice,Smith,active,newsletter;customer,ghl-form:website,2024-03-01,Acme,pro
bob@example.com,Bob,Jones,unsubscribed,newsletter,ghl-import,2023-11-20T09:30:00Z,,
carol@example.com,Carol,,bounced,,ghl-form:webinar,2022-06-15,"Carol & Co, Ltd",
dave@example.com,,,complained,newsletter,,,,
"erin@example.com","Erin","O""Brien",active,"webinar;newsletter",ghl-form:webinar,2024-01-09T14:05:00+01:00,,free
```

Save it as UTF-8. Excel's "CSV UTF-8" works; a byte order mark at the start is fine.

## File rules

- **Header row required.** The first row names the columns. Column names are case-insensitive and may be in any order. Only `email` is required.
- **Delimiter:** comma or semicolon, detected from the header row (whichever appears more often outside quotes). European Excel exports that use `;` work as they are.
- **Quoting:** standard CSV. Quote a field with `"` when it contains the delimiter, a quote or a line break, and double quotes inside it (`"O""Brien"`). Line breaks inside quoted fields are kept. A row with broken quoting (such as `An"n` or `"Bob"x`) is rejected on its own; a quote that is never closed turns the rest of the file into one rejected row.
- **Line endings:** LF or CRLF.
- **Blank rows** are skipped and not counted.
- **Size:** at most 50 MiB and 500 columns.
- Leading and trailing spaces are trimmed from every value.
- An **empty cell means "not given"**: it never clears a value of an existing contact.

## Columns

| Column | Required | Format | Meaning |
|---|---|---|---|
| `email` | yes | an email address | Trimmed and lowercased. Rows without a valid address are rejected. |
| `first_name` | no | text, at most 200 characters | |
| `last_name` | no | text, at most 200 characters | |
| `status` | no | `active`, `unsubscribed`, `bounced` or `complained` (case-insensitive) | Empty means `active` for a new contact and "no change" for an existing one. |
| `tags` | no | tag names separated by `;` | Each tag is 1–64 characters, case-insensitive (`VIP` and `vip` are the same tag). Tags are created as needed. In a file that uses `;` as its delimiter, quote the cell: `"newsletter;customer"`. |
| `consent_source` | no | text, at most 200 characters | Where the consent came from, e.g. the GHL `source` or a form name. Recorded as `import:<consent_source>`. |
| `consent_date` | no | RFC 3339 (`2024-03-01T10:00:00Z`, `2024-01-09T14:05:00+01:00`) or `YYYY-MM-DD` (read as midnight UTC) | When consent was given, e.g. the GHL `dateAdded`. At most one day in the future. |
| `attr.<key>` | no | text | A custom attribute, e.g. `attr.company`. The key is 1–64 letters, digits or underscores and keeps its case. Values are stored as text. All attributes of a contact together must stay under 16 KiB. |

Any other column is ignored and listed in the report's `warnings`, as is an `attr.` column with an invalid key.

## What an import does

**New address:** the contact is created with the row's status, names, attributes and tags, and a consent event of type `import` is written to the consent log with source `import:<consent_source>` (or `import:unspecified`) and the time `consent_date` (or the time of the import). Unsubscribed, bounced and complained contacts are imported too, so they stay blocked if the address signs up again.

**Existing address, `on_existing=update` (the default):**
- non-empty names and attributes in the row replace the stored values; empty cells change nothing;
- tags in the row are added; tags the contact already has are kept (an import never removes tags);
- the status changes only when that makes the contact harder to email (see below);
- a consent event of type `import` is added (see below).

**Existing address, `on_existing=skip`:** names, attributes and tags stay as they are. A status that blocks sending is still applied, because an import must never leave an address sendable that the file says has opted out, and the consent event is still added.

**Consent log (I-2):** every imported row gets a consent event of type `import` with source `import:<consent_source>` (or `import:unspecified`) and the time `consent_date` (or the time of the import): new and existing contacts alike, in both modes, because the consent log is evidence rather than contact data. For an existing contact the event is not repeated when the same import consent is already in the log (same source and date, or the same source when the row has no date), so importing a file again adds nothing.

**Status rules (never reactivate):**
- An import never makes a contact active again. A row that says `active` for a contact that is unsubscribed, bounced, complained or pending leaves it as it is and is counted in `not_reactivated`. Rows for erased or manually suppressed addresses are not counted there: they are rejected with reason `suppressed: …` (see below). Only the contact themselves (a confirmed resubscribe) or an admin can reactivate a contact.
- `active` → `unsubscribed`, `bounced` or `complained` is applied (recorded in the status history with source `import`).
- Between non-active statuses a change is applied only when it makes reactivation harder: `unsubscribed` → `bounced` or `complained`, `bounced` → `complained`. Anything else (for example `bounced` → `unsubscribed`) keeps the stored status and is counted in `status_kept`.

**Rejected rows:** a row is not imported when its address is missing or invalid, when the address was erased (GDPR) or is on the manual suppression list (reason `suppressed: …`), or when a value is invalid (unknown status, bad date, tag too long, and so on). The report lists them by line number, never by address, and a reason never repeats a value from the row (it says what is wrong, e.g. `status must be active, unsubscribed, bounced or complained`), so look the line up in your file.

**A rejected row still blocks its address.** When the address and a status of `unsubscribed`, `bounced` or `complained` are valid but another value of the row is not (a bad `consent_date`, a tag that is too long, attributes over 16 KiB, …), or writing the rest of the row fails, the status is imported anyway: an existing contact moves to it (by the status rules below), and a new address is created with only its address and status (and its consent event). Names, attributes, tags and consent details of that row are not imported. The row is counted in `rejected_count` with a reason ending in `; only the row's status (unsubscribed) was imported`, and the contact in `status_only`. Fix the row and import it again to bring in the rest. An invalid `status` value, a row with too many fields or broken CSV quoting is rejected completely.

**Duplicate addresses:** rows for the same address are merged into one contact: tags are combined, later non-empty values win for names, attributes, `consent_source` and `consent_date`, and the status that blocks sending most wins (`complained` over `bounced` over `unsubscribed` over `active`). The merged rows are counted in `duplicates_merged`; a rejection of the merged contact is reported on its first line. A rejected row's blocking status still counts for the merge: `a@x.com,,active` followed by `a@x.com,,unsubscribed` with a bad date imports the contact as unsubscribed.

**Sequences:** tags added by an import don't start tag-triggered sequences. Enrol imported contacts explicitly (for example with enrol-at-step for contacts that were mid-flow in GHL).

## Sending the file

```bash
# Dry run: evaluates the whole file now and writes nothing (200 with "report")
curl -sS -X POST "$MAILROOM/api/v1/imports?dry_run=true" \
  -H "Authorization: Bearer $TOKEN" \
  -F file=@ghl-export/contacts.csv

# The import itself (202 with "job"); retries with the same Idempotency-Key don't import twice
curl -sS -X POST "$MAILROOM/api/v1/imports?on_existing=update" \
  -H "Authorization: Bearer $TOKEN" -H "Idempotency-Key: import-contacts-1" \
  -F file=@ghl-export/contacts.csv

# Follow it until status is succeeded or failed; the report appears when it is done
curl -sS "$MAILROOM/api/v1/imports/$JOB_ID" -H "Authorization: Bearer $TOKEN"
```

The file can also be sent as JSON (`{"csv": "email,...\n...", "on_existing": "skip"}` with `Content-Type: application/json`) or as the raw body with `Content-Type: text/csv`. `on_existing` can be given as a query parameter or as a form or JSON field; when both are given they must agree.

A file without a header row or without an `email` column, an unreadable CSV or a file over 50 MiB is refused at once with 422 (`field: "file"`; 413 when the request itself is too large), and no job is created.

The import runs as a background job of type `import.contacts` and writes 500 contacts per transaction. When the server restarts during an import, the job continues after the last written batch. The uploaded file is stored under `<data>/imports/` (mode 0600) only while the job runs and is deleted when it finishes.

## The report

```json
{
  "dry_run": false,
  "on_existing": "update",
  "rows": 5,
  "created": 5,
  "updated": 0,
  "unchanged": 0,
  "skipped": 0,
  "duplicates_merged": 0,
  "not_reactivated": 0,
  "status_kept": 0,
  "rejected_count": 0,
  "status_only": 0,
  "rejected": [],
  "status_counts": {"active": 2, "bounced": 1, "complained": 1, "unsubscribed": 1},
  "tag_counts": {"customer": 1, "newsletter": 4, "webinar": 1},
  "warnings": []
}
```

| Field | Meaning |
|---|---|
| `rows` | Data rows in the file. Every row is counted once: `rows = created + updated + unchanged + skipped + duplicates_merged + rejected_count`. |
| `created` | New contacts. |
| `updated` | Existing contacts that changed: names, attributes, tags or a status that blocks sending. |
| `unchanged` | Existing contacts that already matched the row. |
| `skipped` | Existing contacts left alone because `on_existing=skip`. |
| `duplicates_merged` | Rows merged into an earlier row for the same address. |
| `not_reactivated` | Rows saying `active` for a contact that stays non-active (also counted in `updated` or `unchanged`). |
| `status_kept` | Rows whose status was refused because it would make the contact easier to reactivate (also counted in `updated` or `unchanged`). |
| `status_only` | Contacts whose rows were rejected but whose status that blocks sending was still applied (their rows are in `rejected_count`). |
| `rejected_count`, `rejected` | Rows not imported; `rejected` lists the first 1000 as `{"line": 17, "reason": "..."}`. The header is line 1; a quoted field with line breaks spans several lines. |
| `status_counts` | Contacts by their status after the import: those created, updated, unchanged or skipped, plus the `status_only` contacts whose status was applied. |
| `tag_counts` | Imported contacts per tag in the file. |
| `warnings` | Problems that didn't stop the import, such as ignored columns. |

A dry run returns the same report with `"dry_run": true`, computed against the contacts as they are at that moment.
