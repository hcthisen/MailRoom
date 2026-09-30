# Leaving GoHighLevel: your new email setup

**{{AGENCY}}** is closing its GoHighLevel agency account on **{{SHUTDOWN_DATE}}**. Your sub-account closes at the same time, and everything in it is deleted: contacts, tags, workflows and emails. It cannot be restored.

This kit gives you the cheapest way to move on, with a self-hosted setup that you own:

| Part | What it does | Price (approx.) |
|---|---|---|
| **Mailroom** on a small VPS | One app for contacts, tags, sign-up forms, drip sequences, newsletters, unsubscribes and statistics. Everything in it can also be controlled through its API | VPS approx. €5–7/month incl. backups |
| **Amazon SES** | The actual delivery to Gmail, Outlook etc. | approx. $0.10 per 1,000 emails |

Mailroom is open source (MIT licence) and included in this kit as source code. It sends from **one domain**: yours.

**You only create accounts and API keys.** A coding agent (Claude Code or Codex) does the technical work by following [AGENTS.md](AGENTS.md): it exports your GoHighLevel data, creates the server, builds and starts Mailroom, sets up Amazon SES and your DNS records, and moves your contacts, drip sequences and forms. It stops and asks for your "yes" at five checkpoints. After the setup, you or your agent can manage everything in Mailroom through its web interface (e.g. `https://newsletter.yourdomain.com`) or its API.

New Amazon SES accounts start in a test mode (the "sandbox") that can only send to your own domain. The agent asks AWS to lift it for you. AWS usually answers by email within a day, and nothing goes to your contacts until then.

## Start here

➡️ **Follow the step-by-step guide: [guide/steps.md](guide/steps.md)**

## Timeline

| When | What |
|---|---|
| **Now** | Part 1: Export everything from GoHighLevel |
| At least 3 weeks before {{SHUTDOWN_DATE}} | Parts 2–6: Accounts, keys, and start the agent |
| At least 1 week before {{SHUTDOWN_DATE}} | New website forms are live and the new drip sequences are running |
| {{SHUTDOWN_DATE}} | GoHighLevel closes |

## What's in the kit

| File | For |
|---|---|
| [guide/steps.md](guide/steps.md) | You: what to click, step by step. The screenshots (📸) are still to come: [guide/CAPTURE.md](guide/CAPTURE.md) is the brief for capturing them, and until they are added the text alone describes every step |
| `.env.example` → `.env` | You: where your keys and settings go. **Never share `.env`.** |
| `ghl-export/` | You: your exported GoHighLevel data and workflow notes |
| [AGENTS.md](AGENTS.md) | The coding agent: the technical runbook (`CLAUDE.md` points Claude Code to it) |
| [app/](app/) | Mailroom's source code and documentation ([PRD](app/PRD.md), [API spec](app/api/openapi.yaml), [import and sequence formats](app/docs/)) |
| [deploy/](deploy/) | How Mailroom runs on the server: the Docker Compose file and its [README](deploy/README.md) (install, backups, upgrades) |
| [LICENSE](LICENSE) | MIT licence for the whole kit |

## Responsibility

This kit and Mailroom are provided **as-is** (see [LICENSE](LICENSE)). They are one-off transition tools, not a product or service. {{AGENCY}} does not provide setup, operation, updates or support, and accepts no liability. You own your installation. You are the data controller for your contacts, and you are responsible for making sure your emails comply with marketing law and GDPR.

## Legal in brief (not legal advice)

- You may only send marketing to people who have given consent (in Denmark: section 10 of the Marketing Practices Act). Keep records of where and when they signed up. The GHL export is your documentation for the existing contacts.
- Every marketing email, including drip emails, has a one-click unsubscribe link. Mailroom adds it automatically.
- Mailroom records which links each contact clicks. The default consent text on your forms says so; keep it in your privacy policy too.
- Contacts who unsubscribed in GHL are imported as **unsubscribed** and will never receive marketing email again.
- AWS and Hetzner process data on your behalf. AWS's data processing addendum (DPA) is automatically part of their terms. With Hetzner you need to conclude the data processing agreement (AVV) yourself (step 3.4).
