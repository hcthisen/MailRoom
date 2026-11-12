# Migration notes for the deploying agent

Use these notes with [AGENTS.md](../AGENTS.md), especially phases 1 and 7–10. This is a read-only GHL export and a reviewed Mailroom import; it does not authorize changing the source CRM.

## Choose the initial scope before capturing every automation

Confirm whether the owner wants contacts and newsletters first, selected email sequences, or a broader migration proposal. A contact import does not imply replaying old workflows or replacing every form.

Inventory names and publish states first. Prioritize flows containing email, then capture their actual triggers, actions, timing, conditions, active participants and external dependencies. A workflow name is not evidence of its behavior. Historical enrolment totals, current active participants and published status are different facts; a published workflow can have no active participants.

If legacy work is substantial, show the verified inventory and unresolved decisions at checkpoint 1. The owner can approve the contact export and continuing infrastructure work while explicitly deferring selected workflows/forms. Record that boundary in `PROGRESS.md`; deferred work must not silently become an activated sequence. Keep drafts paused and do not call enrolment endpoints. Report uninspected items as inventory, not completed conversions.

When work is delegated, one worker can own the contact export/conversion and another the read-only workflow/form capture. Give each exclusive files and browser tabs. The coordinator owns the combined report, checkpoint approval and actual import; workers return evidence and gaps, not competing copies of `PROGRESS.md`.

## Export once, reconcile independently

1. Validate the token and location with a narrow read. Use the documented API version for each endpoint; do not assume every GHL endpoint uses the same version.
2. Save each raw page before conversion. Follow the endpoint's documented cursor/offset, detect repeated cursors, deduplicate source IDs and stop with an explained error if a page repeats or pagination ends before its advertised total. When following a returned next-page URL, require the expected HTTPS API origin before forwarding the bearer token.
3. Apply bounded retries to read-only `429`/temporary server failures, respecting `Retry-After`. Avoid a tight loop or repeated full exports. Save enough non-secret progress to resume from the last verified page.
4. Distinguish failure causes before changing credentials:

   | Signal | Next step |
   |---|---|
   | Authentication or scope error | Check token type, location, endpoint version and required read scope; never print the token. |
   | Generic/WAF `403` | Inspect a sanitized response. A client without a useful `User-Agent` was rejected during a deployment; a descriptive agent/client header resolved that request. Do not spoof authorization, bypass a challenge, or assume every `403` is this case. |
   | `WORKFLOW_API_NOT_ENABLED_FOR_LOCATION` | Detailed workflow access is feature-gated for that subaccount. Broader scopes or another copy of the same token do not fix that condition. Use read-only browser capture or owner documentation. |

5. Request one independent UI CSV backup and record the screen count. Select all contacts, not just the first visible page. Current HighLevel exports use the table's selected visible columns; make the needed columns visible before export. An asynchronous export is downloaded from **Contacts → Bulk Actions** when complete. If a download-event tool times out, check that export's actual local download before clicking again and producing duplicates.
6. Compare row counts, source contact-ID sets and normalized primary-email sets between UI CSV and API export. Equal totals alone can conceal missing/repeated records. Explain timing changes or missing columns. Preserve full API records, email DND settings and custom fields even when the UI CSV is narrower.

HighLevel documents [visible-column exports and Bulk Actions downloads](https://help.gohighlevel.com/support/solutions/articles/48001238482-how-to-export-contacts-to-a-csv-in-highlevel) and [limited workflow API availability](https://marketplace.gohighlevel.com/docs/ghl/workflows/workflows/index.html). Recheck those pages when the UI/API differs; their availability and versions can change.

## Convert statuses without losing suppression evidence

Use [Mailroom's CSV contract](../app/docs/import-format.md), not the GHL CSV column names, for the destination file.

- Interpret global DND and email-channel DND separately from contact status. In an email `dndSettings` object, `status: active` means that the **DND block is active**, not that the person is an active subscriber. Preserve a global block or active email block.
- A DND reason may reference a workflow that applied the block. Resolve that workflow's actual meaning before classifying it as bounce, complaint or unsubscribe. Build a mapping for the current account; never copy another customer's workflow IDs or infer semantics from an unexplained ID alone.
- A provider reason combining permanent bounce, spam and unsubscribe does not identify which event occurred. Keep the original reason and source ID in the private evidence. Propose a conservative permanent-block mapping, show its count separately and have the owner approve it at checkpoint 1. Never turn ambiguous blocked records into active contacts merely to simplify the report.
- Normalize primary emails for deduplication; do not invent replacement addresses for malformed ones. Report missing-email source records, invalid destination rows and duplicate merges separately. Keep omitted records in the raw backup.
- Merge duplicate-email rows using the strongest blocking status (`complained`, then `bounced`, then `unsubscribed`, then `active`) and the union of tags. Preserve non-empty names/custom values according to the import contract. Report any unmapped manual blacklist or suppression behavior for an explicit handling decision.
- Validate attribute keys, tag lengths, date syntax and file/attribute-size limits before upload. Preserve a source-ID attribute so a rejected row can be traced privately without printing the person's address in the report.
- Carry the requested GHL `source` and `dateAdded` values, but label their meaning honestly: a contact creation date is neither independent proof of email consent nor a verified workflow-entry time. An active source status is not approval to send to everyone. Preserve real consent evidence when available and review the audience before any live send.

Keep `REPORT.md` concise: source/UI reconciliation; omitted/merged/rejected counts; status and tag totals; ambiguous mappings; workflow/form decisions; and what remains outside scope. Save the dry-run response separately, obtain checkpoint 4 approval, then compare the actual import report and dashboard totals with the preview. CSV-imported tags do not start Mailroom sequences; see [import application behavior](../app/internal/imports/apply.go) and [sequence hooks](../app/internal/sequences/hooks.go).

## Capture workflows without editing them

Try the documented read-only definition API when the account supports it. If it is unavailable, the agent can perform the browser capture rather than making the owner transcribe every email. The owner still decides business behavior that cannot be inferred.

- Use the workflow builder's visible UI. It may be inside an iframe; scope browser locators to the observed frame instead of repeatedly searching the outer page.
- Inspect triggers and every relevant node, including wait settings, branch conditions, exit/removal actions and current participants. A wait label may hide an exact clock-time restriction or advanced window.
- Copy the exact subject and HTML for each email action; keep the untouched source alongside converted content. Compare body text against the rendered editor, not just the number of saved files. Note intentionally repeated email bodies.
- Some editors offer autosave introduction, normalization or unsaved-change prompts simply after opening a panel. Keep manual/read-only inspection, discard unintended changes and verify that nothing was saved or published. Do not run a workflow test or submit a source form just to inspect it.
- Record source URLs and fields needed by linked surveys, community invitations, account provisioning or other external services. Do not open or print credential-bearing webhook settings unless needed for an explicitly authorized integration task.

Create a `workflow.md` from [the template](../ghl-export/workflows/_template.md) for each selected email flow. The capture is incomplete until unknowns are marked. Put the smallest owner decision beside the relevant gap:

| Source behavior | Safe handling |
|---|---|
| Past competition, deadline, price or promotion | Archive until the owner approves current replacement content; do not replay historical participants. |
| Conditional branches or removal from another workflow | Preserve the conditions in the proposal. Mailroom sequences are linear; separate targeted sequences require an approved routing/integration plan. |
| AI response, incoming-message variable or webhook output | Not a static drip. An existing attribute can display saved data but cannot generate future AI content. Leave the source integration in place unless replacing it is requested. |
| CRM/customer provisioning, SMS, booking or staff notification | Capture the dependency; migrating its email portion does not recreate those actions. |
| Exact clock-time waits or unverified re-entry rules | Do not replace them with approximate delays/defaults and call that equivalent. State the proposed difference and test the supported behavior. |
| Zero active source participants | No mid-flow cohort is evidenced. Historical totals do not justify new enrolments. |

For a supported linear draft, use [the sequence format](../app/docs/sequence-format.md): explicitly set `status: paused`, record the trigger and exit tags, exact delays, re-entry choice and send window. A missing sequence window inherits `default_send_window`; check the setting before claiming timing equivalence. A paused sequence can accept explicit API enrolments, so paused is not permission to enrol anyone.

Translate merge fields deliberately, for example `{{contact.first_name}}` → `{{first_name}}` and an approved custom field → `{{attr.field_key}}`. Inventory unsupported tokens such as incoming-message or webhook-response variables. Mailroom attributes are escaped values; a historical rich-HTML field may not render like the original editor. Preview the converted result with representative test data, the current footer and unsubscribe layout. Keep assumptions, defaults and unresolved dependencies beside the prepared YAML, then use the import dry run before creating a draft.

## Forms: verify purpose and consent before replacement

Record fields, required inputs, hosted URL, website placement, on-submit behavior, tags, workflow links, consent wording and confirmation behavior. Keep unverified bindings marked unknown. Forms and surveys can be separate source objects; a survey-triggered workflow is not evidence that one of the ordinary forms starts it.

A lead/contact form, support intake, booking request or phone-number checkbox for SMS marketing is not automatically an email-newsletter opt-in. Do not convert those submissions into newsletter enrolments or replace their original business behavior silently. The owner can retain those forms and request a separate Mailroom newsletter form with clear email consent. Keep double opt-in at the documented default unless the owner's chosen behavior calls for confirmation; explain and record that choice. Do not change the website without an explicit request.

## Keep private exports private

Use private directories (mode `700`) and source/data files (mode `600`) on systems supporting Unix permissions; use equivalent access restrictions elsewhere. This kit ignores generated `ghl-export/` artifacts, including HTML, Markdown and screenshots, while keeping its workflow templates tracked. Ignore rules do not protect already tracked files or captures saved elsewhere: check `git check-ignore`/`git status`, or equivalent, before committing. Keep customer exports out of the reusable template repository, and never include source contact data, signed links, webhook credentials or screenshot-visible tokens in a generic guide.

## Verification that proves the migrated behavior

- Use a unique setup tag/segment and unique simulator address labels. Check its dry-run recipient count exactly before a test broadcast; never use all imported active contacts for testing.
- Test-send endpoints are useful for layout and basic delivery. For working tracked links, unsubscribe behavior and received unsubscribe headers, prepare an actual broadcast to the owner/simulator segment, show its dry run and obtain approval for that send under the runbook's checkpoints.
- Distinguish queued, SES accepted, delivered and seen in Inbox/Spam. Inspect a received owner's message or ask the owner; check SPF, DKIM, DMARC and both unsubscribe headers on the actual broadcast. If another mailbox provider is unavailable, report it as untested.
- Test the visible unsubscribe flow and one-click POST with dedicated owner-controlled records; do not change an imported customer's status for a test. Verify simulator contacts become bounced/complained and tracking appears in the contact timeline/statistics. Expected simulator rates are not customer delivery results.
- When confirmation is enabled, verify pending before confirmation and active/tagged only after the actual confirmation submission. Opening the confirmation page alone changes nothing; see [the confirmation handler](../app/internal/forms/confirm_http.go). Test external incoming mail and an approved reply separately from a self-addressed SES message.
- Record final evidence and remaining limits, then take a final backup after contact, layout and form changes. Keep test counts separate from the original import totals and leave deferred sequences inactive.
