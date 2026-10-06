# Capture brief for the computer-use agent

You walk through [steps.md](steps.md) on a demo setup and take every 📸 screenshot it lists. Your output is used to build the client guide as HTML and PDF. You capture only; you don't write the guide text.

## Before you start (operator prepares)

| Item | Why |
|---|---|
| A **demo GHL sub-account** with ~20 fake contacts, 1 workflow with 2–3 emails, and 1 form | Real client data must never appear |
| A **fresh demo AWS account** | So steps 2.1–2.7 look exactly like a new client's |
| A **demo Hetzner account/project** | Same reason |
| A **demo domain that is Active on Cloudflare** | For Option A |
| A **clean browser profile**: English UI, light mode, 100% zoom, window 1440×900, no bookmarks bar, no extensions visible | So all screenshots look the same |
| The repository folder, with Claude Code or Codex installed | For the Part 0 steps (the agent creates `.env` itself) |

Set the UI language to **English** in GHL, AWS, Hetzner and Cloudflare before you start.

## Hand-off to the operator

**Stop and let the operator do it** whenever a step involves:
- creating an account, or any password field,
- payment or card details, phone or ID verification,
- a CAPTCHA, an email verification code, or MFA setup.

After the operator has finished, take the screenshot the step asks for (if any). Never type passwords, card numbers or verification codes yourself.

## How to take each screenshot

1. Get the screen into the state the 📸 line describes. The element to highlight must be visible, and must not be clicked yet (for "click X" steps).
2. Capture the **whole browser window** (or the whole terminal or text-editor window).
3. Save it as `guide/captures/raw/<file name from steps.md>`.
4. Add one entry to `guide/captures/manifest.json`:

```json
{
  "file": "02-05b-user-name.png",
  "step": "2.5",
  "caption": "Name the user setup-agent and leave console access unticked.",
  "highlight": [[x, y, width, height]],
  "blur": [[x, y, width, height]],
  "captured_at": "2026-10-01",
  "ui_note": "optional: anything that differs from steps.md"
}
```

- `highlight` holds the box(es) around what the client should click or look at.
- `blur` holds **every** secret or personal value: tokens, keys, account IDs, location IDs, emails, phone numbers, names and addresses. When in doubt, blur it.
- If the real UI differs from what [steps.md](steps.md) says (menu renamed, extra screen, different button), capture what you actually see and describe the difference in `ui_note`. The guide text is corrected from these notes.

## After the capture

1. **Revoke everything you created:** the GHL Private Integration, the AWS `setup-agent` access key and user, the Hetzner token, and the Cloudflare token. Blurring is the second line of defence; revoking is the first.
2. A script turns `raw/` + `manifest.json` into `guide/captures/final/`: it draws highlight boxes and blurs the regions. The screenshots you capture are never published without this step.
3. The guide is built from **one source**: [steps.md](steps.md) text + `final/` images → an HTML site (one page per Part) and a PDF printed from the same HTML.
4. When a vendor changes its UI, re-run only that Part's capture and rebuild.

## What not to capture

- Anything from Part 6 beyond the terminal screenshots (the agent's run happens on the client's machine).
  Exception: `04b-02-record-table.png`, `06-04-checkpoint-example.png` and `07-01-mailroom-login.png` are taken during {{AGENCY}}'s own pilot install, once Mailroom exists.
- Billing pages, card forms, personal data screens.
- Real client sub-accounts.
