# AGENTS.md: runbook for migrating from GoHighLevel to Mailroom + Amazon SES

You are the coding agent for a customer who is leaving GoHighLevel (GHL). The customer has created accounts and put keys in `.env`, following [guide/steps.md](guide/steps.md). Your job is to export their GHL data, install Mailroom, migrate, test and hand over. The customer is not necessarily technical, so explain briefly and concretely what you are doing and what you need.

**Mailroom** is the single app in [app/](app/). It handles contacts, tags, forms, drip sequences, broadcasts, unsubscribes and SES bounce handling. Read [app/PRD.md](app/PRD.md) for what it does. For how to use it:
- the API spec: [app/api/openapi.yaml](app/api/openapi.yaml) (a snapshot; the running server serves the same spec at `/api/v1/openapi.json`),
- the API guide at `/api/v1/docs` on the running server (no token needed),
- the file formats: [app/docs/import-format.md](app/docs/import-format.md) (contacts CSV) and [app/docs/sequence-format.md](app/docs/sequence-format.md) (sequence YAML),
- how to run it on the server: [deploy/README.md](deploy/README.md).

Target architecture:

```text
Website form ─► Mailroom (one container, SQLite, HTTPS) ─► Amazon SES ─► recipients
                       ▲                                       │
                       └──────── SNS (bounces/complaints) ◄────┘
```

**How you work with Mailroom:**
- The **CLI** is only for provisioning and bootstrap: `mailroom provision ses`, `mailroom provision dns`, `mailroom dns records`, `mailroom doctor`, `mailroom admin create`, `mailroom token create` (plus `backup`, `restore` and `version` for operations). On the server they run in the container, in `/opt/mailroom/`, as `sudo docker compose run --rm mailroom …` (before Mailroom runs) or `sudo docker compose exec mailroom mailroom …` (while it runs). Every command accepts `--json`. Exit codes: 0 ok, 1 error, 2 a check failed or needs a human.
- **Everything else goes through the REST API** at `https://APP_SUBDOMAIN.DOMAIN/api/v1/`, with `Authorization: Bearer <token>` (the admin token from phase 5). Send an `Idempotency-Key` header on every POST, `If-Match` with the `ETag` from the last GET on every PATCH/PUT, and use `?dry_run=true` first where it is offered (imports, enrolments, broadcast send, forms, layout and more; an operation without a dry run answers `422` on `query.dry_run` and changes nothing). After a `400` or `422`, fix the request and send it with a **new** `Idempotency-Key`; reuse a key only to retry the unchanged request after a `5xx`, `429` or timeout. Long work (imports, backups, broadcast sends, large enrolments) answers `202` with a job: poll `GET /api/v1/jobs/{id}` until `status` is `succeeded` or `failed`.
- The web interface is at `https://APP_SUBDOMAIN.DOMAIN/` (it opens `/admin/`). The customer uses it after the handover; you use the API.

---

## Ground rules

1. **Secrets:**
   - Read them from `.env`. Never write them in chat, logs, commits or files outside `.env` and `secrets/`. When a command prints a secret, redirect its output straight into a file in `secrets/` instead of showing it.
   - Store generated values (admin password, API token, SSH key) in `secrets/` (gitignored). On the server, Mailroom's settings live in `/opt/mailroom/.env` (mode 600, owned by the container user uid 65532).
   - Setup keys (the AWS `setup-agent` user, Cloudflare, Hetzner) **never** go into the server's `.env`. Pass them only as environment variables to the single command that needs them, and **never as a value on a command line**: `sudo` logs every command with its arguments to the server's system log, so `-e NAME=value` or `sudo env NAME=value` would leave the key on the server's disk and in its backups. Send the values over ssh's standard input and give `docker compose` only the names (`-e NAME`), as in deploy/README.md §4.
2. **Stop and get the customer's explicit yes** at every `⛔ CHECKPOINT` below. A yes applies to that one checkpoint only.
3. **DNS:** only create new records. Never change or delete an existing record without showing it and getting a yes. This applies especially to SPF, DMARC and MX on the root domain. Mailroom's `provision dns` follows the same rule: it never changes or deletes a record, and it exits with code 2 when an existing record conflicts.
4. **No emails to real recipients** until phase 10 is approved. While the SES account is in the **sandbox** (until AWS grants production access, see phase 3), send tests only to:
   - the SES mailbox simulator: `success@simulator.amazonses.com`, `bounce@simulator.amazonses.com` and `complaint@simulator.amazonses.com`. Add a label for each test, e.g. `bounce+seq1@simulator.amazonses.com`, because Mailroom never emails a bounced or complained contact again.
   - addresses on `DOMAIN`, such as `OWNER_EMAIL` if it is on `DOMAIN`. `provision ses` verifies the whole domain in SES, so every address on it is an allowed sandbox recipient.

   Don't verify other addresses (e.g. a Gmail address) in SES to test with: the sending user `mailroom-sender` may only send on `DOMAIN`, so SES refuses those sends in the sandbox (`not authorized to perform ses:SendRawEmail`). If `OWNER_EMAIL` is not on `DOMAIN`, ask the customer for a test address on `DOMAIN`, or leave the inbox tests until production access is granted. After that, `OWNER_EMAIL` and the customer's own Gmail/Outlook test addresses are fine too.
5. **GHL is read-only.** Never write, delete or change anything in GHL.
6. **One sending domain:** `DOMAIN`. `FROM_EMAIL` and `REPLY_TO_EMAIL` must be on it (Mailroom refuses to start otherwise). If they are not, stop and ask.
7. **Log progress** in `PROGRESS.md`: phase, status and important IDs (no secrets). If you are interrupted, read `PROGRESS.md` and continue from there.
8. **If Mailroom behaves differently from its docs,** trust what it actually does, work around it if it's safe, and note the difference in `OPERATIONS.md`. Mailroom is MIT-licensed and the source is in `app/`, so you may fix a clear bug there. If you do, tell the customer and record the change in `OPERATIONS.md`.

---

## Phase 0: Preflight

- Check the tools you need on this machine, and install what is missing (tell the customer first: on a managed laptop, installing may need admin rights they have to grant or ask their IT for):
  - `ssh` (built into macOS, Linux and Windows 10/11), and `rsync`, or `tar` on Windows (deploy/README.md §1),
  - the AWS CLI v2 (`aws`) and the Hetzner CLI (`hcloud`, only with `HCLOUD_TOKEN`),
  - `curl`, and `jq` if you want it for the API recipes (optional).

  If a CLI can't be installed, call the same services' HTTP APIs with `curl` instead.
- Read `.env`, and check that the required fields are filled in. Either `HCLOUD_TOKEN` or `VPS_HOST` must be set, and with `DNS_MODE=cloudflare`, `CLOUDFLARE_API_TOKEN` must be set.
- Validate each key with one read-only call:
  - GHL: fetch the location (`GET /locations/{GHL_LOCATION_ID}`).
  - AWS (setup keys, `AWS_REGION`): `aws sts get-caller-identity` and `aws sesv2 get-account`. Note whether `ProductionAccessEnabled` is false (sandbox).
  - Hetzner: list servers (`hcloud server list`).
  - Cloudflare: verify the token (`GET /client/v4/user/tokens/verify`) and find the zone for `DOMAIN` (`GET /client/v4/zones?name=DOMAIN`).
- With `DNS_MODE=manual`: look up the nameservers for `DOMAIN`, and tell the customer which provider hosts their DNS.
- Check that `APP_SUBDOMAIN.DOMAIN` and `MAIL_FROM_SUBDOMAIN.DOMAIN` (defaults `newsletter` and `bounce`) don't already exist in DNS. If one does, ask for another name. Also note whether `_dmarc.DOMAIN` has a TXT record (phase 4).
- Check whether `OWNER_EMAIL` is on `DOMAIN` (ground rule 4).
- Check that `GHL_SHUTDOWN_DATE` is in the future. If fewer than 7 days remain, say so clearly. Phase 1 is then the only thing that is urgent.
- Report to the customer: what works, what is missing, and the plan.

## Phase 1: Export from GHL (separate step, highest priority)

This step has nothing to do with Mailroom. It only produces files. Use GHL API v2 (`https://services.leadconnectorhq.com`) with `Authorization: Bearer $GHL_TOKEN` and the `Version` header specified in GHL's documentation.

1. Fetch with pagination and save raw JSON in `ghl-export/raw/`:
   - all contacts with all fields, incl. `tags`, `dnd`, `dndSettings`, `customFields`, `source`, `dateAdded`,
   - custom fields,
   - tags,
   - the list of workflows (name, status),
   - forms.
2. Compare the contact count with `GHL_CONTACT_COUNT` and with `ghl-export/contacts-ui-export.csv` if it exists. Any discrepancy must be explained.
3. Write `ghl-export/contacts.csv` in **Mailroom's import format** ([app/docs/import-format.md](app/docs/import-format.md)). One row per unique email:
   - `email`, `first_name`, `last_name`,
   - `status`: `unsubscribed` if DND/unsubscribed for email, `bounced` if bounced or invalid, `complained` if marked as spam, otherwise `active`,
   - `tags`, separated by `;`,
   - `consent_source` = the GHL `source`, `consent_date` = `dateAdded` (RFC 3339 or `YYYY-MM-DD`),
   - custom fields as `attr.<key>` columns.
   - Drop contacts without an email, and merge duplicates.
4. Read `ghl-export/workflows/` and `_overview.md`, and summarize each flow in a table: trigger, steps, delays, conditions, and what to do with contacts mid-flow. Flag anything unclear.
5. Write `ghl-export/REPORT.md` with counts per status, tags, flows and forms, and what cannot be migrated (SMS, calendars etc.).

⛔ **CHECKPOINT 1:** Show the report. Ask the customer to confirm the numbers and the flow summary and to answer any open questions.

## Phase 2: Server

- **Hetzner** (`hcloud` CLI with `HCLOUD_TOKEN`):
  - Create an SSH key in `secrets/` and upload the public part.
  - Create a server: Ubuntu 24.04, the smallest shared-vCPU type with ≥ 2 GB RAM, in `HCLOUD_LOCATION`, with backups enabled.
  - Create a firewall that only allows inbound 22, 80 and 443.
- **Own server:** Use `VPS_HOST`/`VPS_SSH_USER`, and check OS, RAM and disk space.
- **Hardening:**
  - Create a non-root user with your SSH key, e.g. `admin`. This user is `SERVER` in the commands below (e.g. `admin@203.0.113.10`). Every later step runs `sudo` over a non-interactive `ssh SERVER '…'`, where sudo can't ask for a password, so give the user passwordless sudo: write `admin ALL=(ALL) NOPASSWD:ALL` to `/etc/sudoers.d/90-mailroom` (mode 440) and check it with `visudo -cf /etc/sudoers.d/90-mailroom`.
  - Check that it works **before** locking anything: `ssh SERVER sudo -n true` must succeed (exit 0, no password prompt). Only then disable SSH password login and root login. Otherwise you can lock yourself out, and only the Hetzner rescue console gets you back in.
  - Enable `unattended-upgrades`.
  - Set up `ufw` as an extra layer (allow 22, 80 and 443).
  - Set the timezone to UTC.
- Install Docker Engine and the Compose plugin from Docker's official repository (Docker Engine 27 or newer, for the IPv6 network in the compose file).
- Write the server's IPv4 (and IPv6) to `PROGRESS.md`.

## Phase 3: Build Mailroom and provision SES

Follow [deploy/README.md](deploy/README.md) for the exact commands. Run every `docker compose` command in `/opt/mailroom/` on the server, with `sudo`. The steps are:

1. Copy the kit to `/opt/mailroom/` on the server: `app/` as a directory (with `vendor/`) and the **contents** of `deploy/`, so `docker-compose.yml` sits next to `app/` (deploy/README.md §1).
2. Create `/opt/mailroom/.env` **before** the first `docker compose` command, without an editor (`sudo tee` fed over ssh, deploy/README.md §2), with the customer's sender settings from the local `.env`: `DOMAIN`, `FROM_NAME`, `FROM_EMAIL`, `REPLY_TO_EMAIL`, `COMPANY_FOOTER`, `OWNER_EMAIL`, `TIMEZONE`, `APP_SUBDOMAIN`, `MAIL_FROM_SUBDOMAIN` and `AWS_REGION`. Leave out all setup keys and everything GHL, Hetzner and DNS related. Then give `.env` and a new `data/` directory to the container user, uid 65532, with mode 600 and 700 (deploy/README.md §2). Because `.env` then belongs to that user, run every `docker compose` command with `sudo`.
   - The sender settings (`FROM_*`, `REPLY_TO_EMAIL`, `COMPANY_FOOTER`, `OWNER_EMAIL`, `TIMEZONE`) are copied into Mailroom's settings only once, by the first `doctor` run (phase 4) or the first start. After that, change them with `PATCH /api/v1/settings`, not in `.env`. `COMPANY_FOOTER` (company name and postal address) is required in practice: without it Mailroom sends no broadcast or sequence email.
3. Run `sudo docker compose build`.
4. Run the SES provisioning, passing the setup keys **for this run only**, through ssh's standard input and by name (deploy/README.md §4; ground rule 1):

   ```bash
   printf '%s\n%s\n' "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" | ssh SERVER '
     read -r AWS_ACCESS_KEY_ID; read -r AWS_SECRET_ACCESS_KEY
     export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
     cd /opt/mailroom && sudo --preserve-env=AWS_ACCESS_KEY_ID,AWS_SECRET_ACCESS_KEY \
       docker compose run --rm -T -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY mailroom provision ses --json'
   ```

   It creates (or, when run again, checks and repairs) the SES domain identity with Easy DKIM, the custom MAIL FROM `MAIL_FROM_SUBDOMAIN.DOMAIN`, the configuration set `mailroom`, the SNS topic `mailroom-ses-events`, the backup bucket `mailroom-backup-ACCOUNT-REGION` (left out with `--no-backup-bucket`) and the sending user `mailroom-sender`. It writes `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` of the **sending** user, `AWS_REGION`, `SNS_TOPIC_ARN`, `SES_CONFIGURATION_SET` and `BACKUP_BUCKET` to `/opt/mailroom/.env`, and prints the DNS records (`dns_records` in the JSON; the A/AAAA record comes in phase 4). Save the records, `production_access_enabled` and any `warnings` in `PROGRESS.md`.
   - If it stops before changing anything because it can't write `.env`, fix the owner as in deploy/README.md §2 and run it again.
   - It is safe to run again. If it says the sending user already has a key that is not in `.env`, run it again with `--rotate-keys`.
5. If `production_access_enabled` is false (the SES sandbox), draft the production access text in `secrets/ses-use-case.txt` (200 to 5,000 characters):
   - that contacts opted in with documented consent (from the GHL export),
   - the number of contacts and expected volume,
   - automatic bounce and complaint handling,
   - one-click unsubscribe,
   - that unsubscribed and bounced contacts are suppressed.

⛔ **CHECKPOINT 2:** Show the production access text. After a yes, copy it to the server as `/opt/mailroom/ses-use-case.txt` (mode 644) and submit it with `mailroom provision ses --request-production --use-case /tmp/ses-use-case.txt --json`, mounting the file into the container and passing the setup keys for that run only (deploy/README.md §4). Delete the server copy afterwards. The request is sent as marketing mail for the website `https://DOMAIN`, with `OWNER_EMAIL` as the contact. Tell the customer that AWS replies by email and that they must answer any questions. Continue meanwhile, testing only as ground rule 4 allows.

## Phase 4: DNS

The records are: an A (and AAAA) record for `APP_SUBDOMAIN.DOMAIN` pointing to the server, the 3 DKIM CNAMEs, and the MAIL FROM MX + TXT (SPF). If `_dmarc.DOMAIN` doesn't exist, also propose a monitoring-only DMARC record, and create it only after a yes. With `OWNER_EMAIL` on `DOMAIN` it is `v=DMARC1; p=none; rua=mailto:OWNER_EMAIL`. If `OWNER_EMAIL` is on another domain (e.g. Gmail), Mailroom proposes `v=DMARC1; p=none` without `rua`: reports to another domain need an authorization record there, so receivers would send none. Ask the customer whether they want the reports at an address on `DOMAIN`; if so, add that `rua` to the record by hand.

Both commands read the DKIM tokens from SES with the sending keys in `/opt/mailroom/.env`, so they need no setup keys (only Cloudflare needs its token). Always pass the server's addresses: `--ipv4 IPV4` (and `--ipv6 IPV6` if it has one). Without them the A/AAAA record is left out, and HTTPS fails in phase 5. Add `--dmarc` only for the DMARC record the customer said yes to. Run the commands as in deploy/README.md §5 (`sudo docker compose run --rm mailroom …`).

- **`DNS_MODE=cloudflare`:** Run `mailroom provision dns --cloudflare --ipv4 IPV4 --ipv6 IPV6`, with `CLOUDFLARE_API_TOKEN` for that run only, passed like the AWS keys in phase 3 (over ssh's standard input, `-e CLOUDFLARE_API_TOKEN` by name). Without `--yes` it changes nothing: it prints the records it would create, the ones already present, and any conflicts (existing records it will never change; exit code 2).
- **`DNS_MODE=manual`:** Run `mailroom dns records --ipv4 IPV4 --ipv6 IPV6`, and give the customer the table. Names in it are fully qualified; some DNS providers want only the part before `DOMAIN`. Tell them which provider to log in to (from phase 0). Then wait until they say it's done.

⛔ **CHECKPOINT 3:** For Cloudflare, show the plan and apply it by running the same command with `--yes` after a yes. If there are conflicts, show them and decide with the customer. For manual DNS, the customer's "done" counts as the yes.

Afterwards run `sudo docker compose run --rm mailroom doctor --json` until the DNS checks and the SES identity check pass. Before Mailroom runs, the HTTPS, SNS and backup checks can't pass yet, so the exit code stays 2: read the individual checks. If a record is missing or wrong, tell the customer exactly which one.

## Phase 5: Start Mailroom and bootstrap

1. Run `sudo docker compose up -d`, then `sudo docker compose ps` until `STATUS` shows `healthy`. On start, Mailroom migrates its database, gets its Let's Encrypt certificate for `APP_SUBDOMAIN.DOMAIN` and subscribes `/hooks/ses` to the SNS topic.
2. Run `sudo docker compose exec -T mailroom mailroom admin create --email OWNER_EMAIL` and redirect its output into `secrets/admin.txt`: it prints the generated password once.
3. Run `sudo docker compose exec -T mailroom mailroom token create --name setup-agent --scope admin` and redirect its standard output into `secrets/api-token.txt`: it prints only the token (`mr_…`), once. From your machine, for example:

   ```bash
   ssh SERVER 'cd /opt/mailroom && sudo docker compose exec -T mailroom mailroom token create --name setup-agent --scope admin' > secrets/api-token.txt
   ```

   **From here on, use the API.**
4. Check the token with `GET /api/v1/me`.
5. Check `GET /api/v1/system/doctor`: the HTTPS certificate, the SNS subscription and the SES identity must be `ok`. The backup check warns until the first backup (phase 6), and the production access check stays unhappy until AWS approves.

## Phase 6: Configure through the API

- `GET /api/v1/settings`: the sender, footer, owner and timezone were copied from the server's `.env`. `PATCH /api/v1/settings` (with `If-Match`) only what the customer wants different, e.g. `alert_email`. Click tracking (`tracking.clicks`) stays on; open tracking (`tracking.opens`) stays off unless the customer asks for it.
- The base layout: upload the logo with `POST /api/v1/media`, then `PUT /api/v1/layout` with its `logo_url`, `brand_color` and, if needed, `custom_html`. Reuse the design from `ghl-export/templates/` if it exists. A `custom_html` layout must contain `{{content}}`, a link to `{{unsubscribe_url}}` and `{{footer}}` (the company footer from the settings), both visible: Mailroom refuses them inside hidden blocks (`display:none` and the like), which pasted GHL designs sometimes have. Replace any address typed into the GHL design with `{{footer}}`. Check it with `POST /api/v1/layout/preview`. Then create a draft email with `POST /api/v1/emails` and send it with `POST /api/v1/emails/{id}/test` to a test address (ground rule 4; without `to` it goes to the owner).
- Check that `GET /api/v1/system/backups` shows the nightly backup schedule (02:30 in the settings timezone) and the S3 bucket. Trigger one with `POST /api/v1/system/backups`, poll `GET /api/v1/jobs/{id}` until it has succeeded, and check that the result has an `s3_key`.

## Phase 7: Import contacts

1. `POST /api/v1/imports?dry_run=true` with `ghl-export/contacts.csv` (multipart field `file`). The dry run writes nothing and answers `200` with the `report` right away; there is no job to poll. Show the report: counts per status and tag, merged duplicates, and rejected rows with the line number and reason.

⛔ **CHECKPOINT 4:** Approve the import.

2. Run the same import without `dry_run` (with an `Idempotency-Key`). It answers `202` with a `job`: poll `GET /api/v1/imports/{job_id}` until `status` is `succeeded`, and compare its report with the dry run. Then check the totals with `GET /api/v1/stats/dashboard` (`contacts_total`, `contacts_active`) and spot-check with `GET /api/v1/contacts?status=unsubscribed` (and `bounced`, `complained`).

## Phase 8: Sequences

Nothing in this phase may email a real contact (ground rule 4): real contacts are enrolled only after CHECKPOINT 5.

1. For each flow in `ghl-export/workflows/`, write a sequence file in Mailroom's format ([app/docs/sequence-format.md](app/docs/sequence-format.md)):
   - `status: paused`: a paused sequence sends nothing, and tag triggers don't enrol into it,
   - one step per email, from the copied content,
   - the delays,
   - the trigger: a tag (`trigger.tag`), or none when a form starts it (the form's `sequence_id`, phase 9),
   - the exit tags.
2. Import each file with `POST /api/v1/sequences/import?dry_run=true` (JSON: `yaml` plus the body files in `files`, no `Idempotency-Key`), then without `dry_run` (with an `Idempotency-Key`).
3. Test each sequence while **no real contact is enrolled** in it:
   - Temporarily shorten the delays with `PATCH /api/v1/sequences/{id}/steps/{step_id}` (`delay_seconds`). A delay change moves every waiting enrollment by the difference, which is why this happens before any real enrolment.
   - Create test contacts with `POST /api/v1/contacts/upsert` (ground rule 4 addresses only), and enrol them with `POST /api/v1/sequences/{id}/enrollments` or by adding the trigger tag with `POST /api/v1/contacts/{id}/tags`.
   - Resume the sequence for the test (`POST /api/v1/sequences/{id}/resume`).
   - Check with `GET /api/v1/contacts/{id}/enrollments` and `GET /api/v1/contacts/{id}/timeline` that every step is sent, the exit tag stops the flow, unsubscribing stops it, and a simulator bounce makes the contact `bounced`.
   - To look at one step's email alone, use `POST /api/v1/sequences/{id}/steps/{step_id}/test`. Test sends have no working unsubscribe or tracking links.
   - Afterwards pause the sequence again (`POST /api/v1/sequences/{id}/pause`), restore the delays, and unenrol the test contacts that are still in it (`DELETE /api/v1/sequences/{id}/enrollments/{enrollment_id}`).
4. Contacts mid-flow: prepare the enrolment, but **don't run it yet**. Follow the customer's choice in each `workflow.md`. For "continue", calculate the step from `consent_date` and the delays. `consent_date` is GHL's `dateAdded`, only a stand-in for when the contact entered the workflow, so say so in the plan. Mailroom accepts a `start_at` in the past, which makes the step due at once: when the computed due time is in the past, set `start_at` so the step falls due no earlier than the day after go-live, and spread a large cohort over several days rather than sending it all at once. Run only the dry run now, `POST /api/v1/sequences/{id}/enrollments?dry_run=true` with `emails`, `at_step`, `start_at` and `source: "migration"`, and keep the plan (per sequence: `would_enroll`, `skipped`, and each group's step and `start_at`) for CHECKPOINT 5.

## Phase 9: Forms

1. For each form in `_overview.md`, create a Mailroom form with `POST /api/v1/forms`: `name`, `fields`, `tags`, `sequence_id`, `consent_text`, `redirect_url` and, if the website posts with JavaScript, `allowed_origins`. Double opt-in (`double_optin`) stays **off** unless the customer asks for it.
2. Get the embed code with `GET /api/v1/forms/{id}/snippet`. It also returns the hosted form URL (`hosted_url`, `https://APP_SUBDOMAIN.DOMAIN/f/PUBLIC_ID`).
3. Give both to the customer in `FORMS.md`. The customer or their web agency puts them on the website. Don't change the website unless the customer gives you access and asks you to.
4. Submit each form once with a test address (ground rule 4), e.g. on the hosted page, and check with `GET /api/v1/forms/{id}/submissions`, `GET /api/v1/contacts?q=ADDRESS` and `GET /api/v1/contacts/{id}/enrollments` that the contact, tags and sequence enrolment appear.

## Phase 10: Verification

- `GET /api/v1/system/doctor` must be all green (`ok`). The one exception: when the customer declined the DMARC record in phase 4 (and `_dmarc.DOMAIN` has none), `dns_dmarc` stays `warn`. Tell the customer at CHECKPOINT 5 that Gmail and Yahoo require DMARC from bulk senders, and that a monitoring-only record (`p=none`) changes nothing for their mail.
- Send a real broadcast to test contacts only: tag the test contacts (ground rule 4) `mailroom-test`, create a segment on that tag with `POST /api/v1/segments`, create a broadcast with `POST /api/v1/broadcasts` and `audience` `{"type": "segment", "segment_id": N}`, check with `POST /api/v1/broadcasts/{id}/send?dry_run=true` that `recipients` is exactly the number of test contacts, then send it. Test sends (`/test`) are not enough here, because their unsubscribe and tracking links don't work.
- In the inbox of a test address on `DOMAIN` (and a Gmail and an Outlook address once production access is granted), check the headers of the broadcast and of a sequence step:
  - `dkim=pass` (d=DOMAIN),
  - `spf=pass` (MAIL FROM domain),
  - `dmarc=pass` (with no DMARC record the result is `dmarc=none` instead: report it as above),
  - `List-Unsubscribe` and `List-Unsubscribe-Post` present.
- Test unsubscribing, both from the link and with a one-click POST to the `List-Unsubscribe` URL (body `List-Unsubscribe=One-Click`). Check the contact's status with `GET /api/v1/contacts/{id}`.
- Test bounce and complaint through the simulator addresses in the segment, and check that the contacts become `bounced` and `complained`.
- Click a link in the test mail and check that the click shows up on the contact (`GET /api/v1/contacts/{id}/timeline`). `GET /api/v1/broadcasts/{id}/stats` shows the totals.
- Check with `GET /api/v1/system/ses` that `account.production_access_enabled` is true, and read the quota (`account.max_24_hour_send`, `account.max_send_rate`).

⛔ **CHECKPOINT 5:** Show the results, and the mid-flow enrolment plan from phase 8 step 4 (per sequence: how many contacts, at which step, from when). Only after this may anything be sent to real recipients.

**Go live with the sequences** (after the yes):
1. Resume each sequence the customer approved: `POST /api/v1/sequences/{id}/resume`. From then on, forms and tag triggers enrol new contacts.
2. Run the approved mid-flow enrolments without `dry_run` (with an `Idempotency-Key`), and compare the answer with the dry run. More than 500 contacts answer `202` with a job.

**First real broadcast** (if the customer wants one now):
- A broadcast only goes to `active`, unsuppressed contacts. Check the count with `POST /api/v1/broadcasts/{id}/send?dry_run=true` first.
- Consider starting with a segment of the newest 20–30 % and sending the rest the next day.
- Mailroom's circuit breaker pauses the broadcast automatically at 5 % hard bounces or 0.1 % complaints (settings `circuit_breaker`).

## Phase 11: Handover

1. Write `OPERATIONS.md` for a non-technical reader:
   - the Mailroom URL (`https://APP_SUBDOMAIN.DOMAIN/`), and that passwords belong in a password manager,
   - how to send a newsletter,
   - how to change an email or a delay in a sequence,
   - how to add a form,
   - how to ask a coding agent to do things through the API (point to `/api/v1/docs`),
   - backup and restore, including the key file `data/secret.key`, `data/media/` and a safe copy of the server's `/opt/mailroom/.env` (deploy/README.md, "Backups and disaster recovery"), and that a restore starts with all sending paused: check the carried-over opt-outs, then resume (deploy/README.md, "What a restore does"),
   - what to do if Mailroom or AWS sends a warning,
   - how to rebuild or upgrade (`sudo docker compose build && sudo docker compose up -d`; after changing `.env`, `sudo docker compose up -d`, since a restart keeps the old settings),
   - the monthly cost.
2. Don't print the admin password (ground rule 1: the chat is kept in the session transcript). Tell the customer to open `secrets/admin.txt` in VS Code, copy the password into their password manager with the address `https://APP_SUBDOMAIN.DOMAIN/`, and then delete the file.
3. API token: ask whether they want to keep the `setup-agent` token for future agent work. If not, find its ID with `GET /api/v1/tokens` and revoke it with `DELETE /api/v1/tokens/{id}` as your last API call.
4. Setup keys: the customer turns them off (guide step 7.3). The keys can't do this themselves. Remind them of each one:
   - deactivate, then delete, the access key of the IAM user `setup-agent` (for later setup work, create a new key rather than switching an old one on again),
   - delete the Cloudflare token,
   - delete the Hetzner token. The server keeps running.
   - The GHL token dies with the account.
5. List the GHL-related DNS records (e.g. LeadConnector/Mailgun DKIM, tracking CNAMEs, GHL entries in SPF) that can be deleted **after** `GHL_SHUTDOWN_DATE`. Don't delete them yourself.
6. Update `PROGRESS.md` to "done", and give a short summary.
