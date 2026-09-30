# Sequence format (YAML)

A drip sequence can be written as one YAML file plus, optionally, one file per email body. This is the format of PRD requirement I-3. Use it to move a GoHighLevel workflow into Mailroom, to keep sequences under version control, or to copy a sequence between installs.

- **Import:** `POST /api/v1/sequences/import` (run it with `?dry_run=true` first).
- **Export:** `GET /api/v1/sequences/{id}/export`.

An import creates the sequence, one email per step and the steps in a single transaction. Either all of it is created or none of it.

## Example

`community-welcome.yaml`:

```yaml
name: Community welcome
trigger:
  tag: joined-community
exit_tags: [customer]
exit_add_tag: community-drip-exited
reentry: never
send_window:
  days: [mon, tue, wed, thu, fri]
  start: "08:00"
  end: "20:00"
steps:
  - delay: "0"
    subject: Welcome to the community, {{first_name | "friend"}}!
    preheader: Here is what happens next
    body_file: 01-welcome.md
  - delay: "1d"
    subject: How to get your first win
    body_file: 02-first-win.md
  - delay: "36h"
    subject: Meet the others
    body: |
      Hi {{first_name | "there"}},

      Introduce yourself in the #hello channel. People are friendly.
  - delay: "2d"
    subject: A small gift
    format: html
    body_file: 04-gift.html
```

`01-welcome.md`:

```markdown
Hi {{first_name | "there"}},

Thanks for joining. Over the next week you'll get four short emails.
```

This sequence enrols a contact as soon as the tag `joined-community` is added to them, for example by a form. It sends the welcome email right away, and the next emails one day, 36 hours and two days after the previous one. Sending only happens Monday to Friday between 08:00 and 20:00. A contact who is tagged `customer` leaves the sequence before their next email and is tagged `community-drip-exited`.

## Fields

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | Unique name, at most 100 characters. Importing a name that already exists fails with 409 `conflict`. |
| `status` | no | `active` (default) or `paused`. A paused sequence still accepts enrolments but sends nothing until it is resumed. |
| `trigger.tag` | no | Adding this tag to an **active** contact enrols them. Omit `trigger` for sequences that are only started by a form or through the API. |
| `exit_tags` | no | List of tags. A contact who has any of them leaves the sequence before their next step, and can't be enrolled while they have one. The trigger tag can't also be an exit tag. At most 20. |
| `exit_add_tag` | no | Tag added to a contact when they leave because of an exit tag or because they are no longer active (unsubscribed, bounced, complained or suppressed). It can trigger another sequence. It can't be the trigger tag. |
| `reentry` | no | `never` (default): a contact is enrolled at most once, ever. `after_completion`: enrolled again only if their last enrolment completed; a contact who left early can't come back. `always`: enrolled again whenever they aren't enrolled right now. |
| `send_window` | no | Steps that fall due outside the window wait for its next opening. See [Send window](#send-window). |
| `steps` | yes | 1 to 100 steps, in order. |

Tags are matched case-insensitively and created if they don't exist yet.

### Steps

| Field | Required | Meaning |
|---|---|---|
| `delay` | yes | The wait after the previous step was sent. For the first step it is the wait after the contact is enrolled. See [Delays](#delays). |
| `subject` | yes | Subject line, at most 300 characters. May use merge fields. |
| `preheader` | no | Preview text that inboxes show after the subject. |
| `format` | no | `markdown` (default) or `html`. Use `html` for raw HTML copied from GHL. |
| `body` | one of the two | The email content, inline. A YAML block scalar (`body: |`) keeps line breaks. |
| `body_file` | one of the two | The name of a file in the request's `files` that holds the content. |

Merge fields work in the subject, preheader and body: `{{first_name}}`, `{{last_name}}`, `{{email}}` and `{{attr.<key>}}`. Each can have a fallback, for example `{{first_name | "there"}}`. Every email is wrapped in the base layout with the unsubscribe footer when it is sent.

### Delays

A delay is `0`, or a whole number followed by one unit:

| Unit | Meaning | Example |
|---|---|---|
| `m` | minutes | `90m` |
| `h` | hours | `36h` |
| `d` | days (24 hours) | `2d` |
| `s` | seconds (mainly for tests) | `45s` |

The longest delay is 365 days. Combinations such as `1d12h` aren't accepted: write `36h`. Quote delays (`"0"`, `"2d"`) so every YAML tool reads them as text. An unquoted `0` also works.

Delays count from the moment the previous step was **queued**. If a step waited for the send window, the next delay counts from when it actually went out.

### Send window

```yaml
send_window:
  days: [mon, tue, wed, thu, fri]   # mon tue wed thu fri sat sun (or full names)
  start: "08:00"                    # 24-hour HH:MM, inclusive
  end: "20:00"                      # exclusive; must be after start
```

The window uses the install's time zone (`timezone` in the settings), with daylight saving time handled. On the day clocks change, 08:00 still means 08:00 local time. A sequence without a `send_window` uses the default send window from the settings (`default_send_window`). If that is empty too, the sequence sends at any time.

## Importing

The request body carries the YAML as a string and the body files by name:

```bash
curl -sS -X POST "$MAILROOM/api/v1/sequences/import?dry_run=true" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  --data @- <<EOF
{
  "yaml": $(jq -Rs . < community-welcome.yaml),
  "files": {
    "01-welcome.md": $(jq -Rs . < 01-welcome.md),
    "02-first-win.md": $(jq -Rs . < 02-first-win.md),
    "04-gift.html": $(jq -Rs . < 04-gift.html)
  }
}
EOF
```

- With `dry_run=true`, everything is validated and the response (200) shows the sequence that would be created. Nothing is stored and the IDs in the response aren't kept.
- Without it, the response is 201 with the new sequence, its steps and an `ETag`. Send the real import with an `Idempotency-Key` (e.g. `-H "Idempotency-Key: import-community-welcome-1"`), and never send that key with the dry run: a dry run's answer is stored under its key too, so reusing the key for the real import answers 422.
- Every file in `files` must be used by a step's `body_file`. File names can't contain slashes.
- Unknown keys are rejected, so a typo like `exit_tag:` doesn't go unnoticed.

Validation errors are 422 with a `field` that points at the problem:

```json
{"error": {"code": "validation", "field": "steps[2].delay", "message": "delay \"3 weeks\" must be 0 or a whole number with a unit s, m, h or d (e.g. 90m, 36h, 2d)"}}
```

Typical fields are `yaml` (syntax, unknown keys, more than one document), `name`, `trigger.tag`, `exit_tags`, `send_window.days[1]`, `steps[0].subject`, `steps[0].body_file` and `files.03-offer.md`. Step indexes start at 0.

## Exporting

`GET /api/v1/sequences/{id}/export` returns

```json
{"yaml": "# Mailroom sequence. Format: docs/sequence-format.md\nname: ...", "files": {"01-welcome-to-the-community-first-name.md": "Hi ..."}}
```

In the export, each step's body is in its own file named `<position>-<subject>.md` (or `.html`). All fields are written out, including the defaults (`status`, `reentry` and each step's `format`). Importing an export recreates the same sequence. To make a copy on the same install, change the `name` first.

## After the import

- **Contacts mid-drip in GHL:** enrol them explicitly at the step they had reached. Tags that arrive through a CSV import never start a sequence, so a migration can't send the welcome email to the whole list by accident.

  ```bash
  curl -sS -X POST "$MAILROOM/api/v1/sequences/$ID/enrollments?dry_run=true" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d '{"emails": ["alice@example.com"], "at_step": 3, "start_at": "2026-10-01T08:00:00Z", "source": "migration"}'
  ```

  The step at `at_step` is due at `start_at` plus that step's delay. After that, the remaining steps follow with their own delays. The response counts the contacts that would be enrolled (`would_enroll`) and the skipped ones by reason (`not_found`, `not_active`, `already_active`, `reentry`, `exit_tag`, `no_steps`). Requests for more than 500 contacts run as a job (202).
- **Test:** `POST /api/v1/sequences/{id}/steps/{step_id}/test` sends one step to the owner's address (or to up to 5 addresses in `to`).
- **Change a delay or an email later:** `PATCH /api/v1/sequences/{id}/steps/{step_id}` with `delay_seconds` or `subject`/`body`. Content changes apply to emails not sent yet. A new delay also moves the due time of contacts waiting for that step.
- **Stats:** `GET /api/v1/sequences/{id}/stats`.
